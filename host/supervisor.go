package host

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
)

// reloadDirEnv names the directory the development image unpacks the app's source into.
const reloadDirEnv = "HOLON_RELOAD_DIR"

type supervisor struct {
	root      string
	transport *http.Transport
	turn      chan struct{}
	app       atomic.Pointer[child]
	written   []string
	installed runtimecontract.Install
}

// Run serves PORT and proxies it to the app, which it starts in the source directory with a loopback HOST and PORT of
// the app's own. The app starts from the overlay the platform recorded, or else from the first reload.
func Run(ctx context.Context) error {
	root := os.Getenv(reloadDirEnv)
	if !filepath.IsAbs(root) {
		return errors.New(reloadDirEnv + " must name the source directory")
	}
	// A writable volume outlives the container, and overlays from before this boot are of no use.
	if err := errors.Join(os.RemoveAll(root), os.MkdirAll(root, 0o700)); err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	s := &supervisor{root: root, transport: transport, turn: make(chan struct{}, 1)}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", ":"+cmp.Or(os.Getenv("PORT"), "3000"))
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: s, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	err = s.boot(ctx)
	if err == nil {
		select {
		case err = <-served:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	_ = server.Close()
	s.stop()
	return err
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == runtimecontract.ReloadPath {
		s.reload(w, r)
		return
	}
	app := s.app.Load()
	if app == nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Application restarting", http.StatusServiceUnavailable)
		return
	}
	app.proxy.ServeHTTP(w, r)
}

// boot starts the app from the overlay the platform recorded. Without one, or while the registry does not hold it
// yet, the app waits for the first reload.
func (s *supervisor) boot(ctx context.Context) error {
	recorded, _ := os.ReadFile(os.Getenv(runtimecontract.OverlayFileEnv))
	if len(recorded) == 0 {
		return nil
	}
	var record runtimecontract.OverlayRecord
	if protocol.DecodeJSONNonNull(recorded, &record) != nil || record.Blob() == nil {
		return errors.New("invalid " + runtimecontract.OverlayFileEnv)
	}
	s.turn <- struct{}{}
	defer func() { <-s.turn }()
	found, err := s.fetch(ctx, record)
	if err != nil || !found {
		return err
	}
	if status, reply := s.launch(ctx); status != http.StatusOK {
		fmt.Fprint(os.Stderr, reply.Output)
		fmt.Fprintln(os.Stderr, "application did not start:", reply.Error)
	}
	return nil
}

// reload replaces the app with one running from the posted overlay, and answers once it accepts connections. An app
// that does not start stays down until the next reload.
func (s *supervisor) reload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.turn <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.turn }()
	s.stop()
	if _, err := s.unpack(r.Body); err != nil {
		writeReply(w, http.StatusBadRequest, runtimecontract.ReloadReply{Error: err.Error()})
		return
	}
	status, reply := s.launch(r.Context())
	writeReply(w, status, reply)
}

func (s *supervisor) launch(ctx context.Context) (int, runtimecontract.ReloadReply) {
	var output tail
	var plan runtimecontract.Plan
	data, err := os.ReadFile(filepath.Join(s.root, runtimecontract.PlanFile))
	if err == nil {
		err = protocol.DecodeJSONStrict(data, &plan)
	}
	if err == nil && !reflect.DeepEqual(plan.Install, s.installed) {
		s.installed = runtimecontract.Install{}
		if err = s.run(ctx, plan.Install.Commands, &output); err == nil {
			s.installed = plan.Install
		}
	}
	if err == nil {
		err = s.run(ctx, plan.Prepare, &output)
	}
	if err != nil {
		return http.StatusInternalServerError, runtimecontract.ReloadReply{Error: "the install or build failed: " + err.Error(), Output: output.String()}
	}
	c, err := s.start(plan.Start)
	if err != nil {
		return http.StatusInternalServerError, runtimecontract.ReloadReply{Error: err.Error()}
	}
	status, problem := c.ready(ctx)
	if status != http.StatusOK {
		c.stop()
		return status, runtimecontract.ReloadReply{Error: problem}
	}
	s.app.Store(c)
	go func() {
		<-c.done
		if s.app.CompareAndSwap(c, nil) {
			fmt.Fprintln(os.Stderr, "application exited:", c.command.ProcessState)
		}
	}()
	return status, runtimecontract.ReloadReply{Output: output.String()}
}

func (s *supervisor) run(ctx context.Context, commands [][]string, output io.Writer) error {
	for _, argv := range commands {
		command, err := s.command(ctx, argv)
		if err != nil {
			return err
		}
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			return err
		}
	}
	return nil
}

func (s *supervisor) command(ctx context.Context, argv []string, env ...string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("the plan has an empty command")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir, command.Env = s.root, append(append(os.Environ(), "PWD="+s.root), env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	return command, nil
}

func (s *supervisor) stop() {
	if c := s.app.Swap(nil); c != nil {
		c.stop()
	}
	// Connections to a previous child are dead, and a request that reuses one cannot always be retried.
	s.transport.CloseIdleConnections()
}

type child struct {
	command *exec.Cmd
	address string
	proxy   http.Handler
	done    chan struct{}
}

func (s *supervisor) start(argv []string) (*child, error) {
	address, err := loopbackAddress()
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(address)
	command, err := s.command(context.Background(), argv, "HOST=127.0.0.1", "PORT="+port)
	if err != nil {
		return nil, err
	}
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start application: %w", err)
	}
	target := &url.URL{Scheme: "http", Host: address}
	c := &child{command: command, address: address, done: make(chan struct{}), proxy: &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.Host = p.In.Host
			for _, name := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
				p.Out.Header[name] = p.In.Header[name]
			}
		},
		Transport: s.transport, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Application unavailable", http.StatusBadGateway)
		},
	}}
	go func() {
		_ = command.Wait()
		close(c.done)
	}()
	return c, nil
}

// ready waits until the app accepts connections, and fails once it exits or ctx ends first.
func (c *child) ready(ctx context.Context) (int, string) {
	for {
		connection, err := new(net.Dialer).DialContext(ctx, "tcp", c.address)
		if err == nil {
			_ = connection.Close()
			return http.StatusOK, ""
		}
		select {
		case <-c.done:
			return http.StatusInternalServerError, "application exited before ready: " + c.command.ProcessState.String()
		case <-ctx.Done():
			return http.StatusGatewayTimeout, "application did not become ready"
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// stop asks the app's process group to end, kills what remains of it after five seconds, and waits for the app.
func (c *child) stop() {
	group := -c.command.Process.Pid
	_ = syscall.Kill(group, syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
	}
	_ = syscall.Kill(group, syscall.SIGKILL)
	<-c.done
}

// loopbackAddress reserves a free loopback port for the app.
func loopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String(), nil
}

func writeReply(w http.ResponseWriter, status int, reply runtimecontract.ReloadReply) {
	w.Header().Set(protocol.HeaderContentType, protocol.MIMEApplicationJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(reply)
}

const tailBytes = 4 << 10

// tail keeps the end of what was written to it: at most tailBytes, from a line's start once it has dropped any.
type tail struct {
	mu   sync.Mutex
	data []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if extra := len(t.data) - tailBytes; extra > 0 {
		t.data = t.data[extra:]
		if _, rest, found := bytes.Cut(t.data, []byte("\n")); found {
			t.data = rest
		}
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
