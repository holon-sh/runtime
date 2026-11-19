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
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/opencontainers/go-digest"
)

// reloadDirEnv names the directory the development image unpacks the app's source into.
const reloadDirEnv = "HOLON_RELOAD_DIR"

type supervisor struct {
	root      string
	transport *http.Transport
	turn      chan struct{}
	app       atomic.Pointer[child]
	record    *runtimecontract.OverlayRecord
	written   []string
	unpacked  digest.Digest
	installed *runtimecontract.Install
}

// Run serves PORT and proxies it to the app, which it starts in the source directory with a loopback HOST and PORT of
// the app's own. The app starts from the overlay the platform recorded, or else from the first reload.
func Run(ctx context.Context) error {
	root := os.Getenv(reloadDirEnv)
	if !filepath.IsAbs(root) {
		return errors.New(reloadDirEnv + " must name the source directory")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	s := &supervisor{root: root, transport: transport, turn: make(chan struct{}, 1)}
	// A writable volume outlives the container, and overlays from before this boot are of no use.
	if err := s.empty(); err != nil {
		return err
	}
	recorded, _ := os.ReadFile(os.Getenv(runtimecontract.OverlayFileEnv))
	if len(recorded) != 0 {
		s.record = new(runtimecontract.OverlayRecord)
		if protocol.DecodeJSONNonNull(recorded, s.record) != nil || s.record.Location() == nil {
			return errors.New("invalid " + runtimecontract.OverlayFileEnv)
		}
	}
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
	defer s.stop()
	defer server.Close()
	if err := s.boot(ctx); err != nil {
		return err
	}
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && (r.URL.Path == runtimecontract.ReloadPath || r.URL.Path == runtimecontract.BuildPath) {
		select {
		case s.turn <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		defer func() { <-s.turn }()
		status, reply := s.update(r)
		w.Header().Set(protocol.HeaderContentType, protocol.MIMEApplicationJSON)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
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

// update streams a clean build or replaces the running app from a new source overlay.
func (s *supervisor) update(r *http.Request) (int, runtimecontract.BuildReply) {
	var body io.Reader = r.Body
	var identity digest.Digest
	var build *runtimecontract.Build
	if r.URL.Path == runtimecontract.BuildPath {
		s.stop()
		if err := s.empty(); err != nil {
			return failure(http.StatusInternalServerError, err.Error())
		}
		build = new(runtimecontract.Build)
	} else {
		overlay, err := io.ReadAll(body)
		if err != nil {
			return failure(http.StatusBadRequest, "read source: "+err.Error())
		}
		identity = digest.FromBytes(overlay)
		if s.app.Load() != nil && s.unpacked == identity {
			return http.StatusOK, runtimecontract.BuildReply{}
		}
		s.stop()
		body = bytes.NewReader(overlay)
	}
	if err := s.unpack(body); err != nil {
		return failure(http.StatusBadRequest, err.Error())
	}
	s.unpacked = identity
	return s.execute(r.Context(), build, r.Header.Get(runtimecontract.BuildTokenHeader))
}

func failure(status int, message string) (int, runtimecontract.BuildReply) {
	return status, runtimecontract.BuildReply{ReloadReply: runtimecontract.ReloadReply{Error: message}}
}

var buildEnvironment = []string{"SOURCE_DATE_EPOCH=0"}

func (s *supervisor) execute(ctx context.Context, build *runtimecontract.Build, token string) (int, runtimecontract.BuildReply) {
	var plan runtimecontract.Plan
	err := s.decode(runtimecontract.PlanFile, &plan)
	var env []string

	if build != nil {
		switch err := errors.Join(err, s.decode(runtimecontract.BuildFile, build)); {
		case err != nil:
			return failure(http.StatusBadRequest, err.Error())
		case build.Freeze == nil:
		case s.record == nil:
			return failure(http.StatusBadRequest, "no repository is recorded for this app")
		case !protocol.IsValidDigestImage(os.Getenv(runtimecontract.BaseImageEnv)):
			return failure(http.StatusBadRequest, runtimecontract.BaseImageEnv+" must name the image this pod runs by digest")
		}
		env = buildEnvironment
		defer func() { s.installed = nil }()
	}
	var output tail
	if err == nil {
		err = s.install(ctx, plan.Install, &output, env...)
	}
	if err == nil {
		err = s.run(ctx, plan.Prepare, &output, env...)
	}
	if err != nil {
		return http.StatusInternalServerError, runtimecontract.BuildReply{ReloadReply: runtimecontract.ReloadReply{Error: "the install or build failed: " + err.Error(), Output: string(output)}}
	}

	if build != nil {
		return s.finish(ctx, build, token)
	}
	return s.start(ctx, plan.Start, string(output))
}

func (s *supervisor) finish(ctx context.Context, build *runtimecontract.Build, token string) (int, runtimecontract.BuildReply) {
	var reply runtimecontract.BuildReply
	if build.Freeze != nil {
		image, err := s.freeze(ctx, token, build.Freeze.Dependencies)
		if err != nil {
			return failure(http.StatusInternalServerError, "freeze the app: "+err.Error())
		}
		reply.Image = image.String()
	}
	if len(build.Run) != 0 {
		command := s.command(ctx, build.Run, buildEnvironment...)
		var stderr tail
		command.Stderr = &stderr
		stdout, err := command.Output()
		if command.ProcessState == nil {
			return failure(http.StatusInternalServerError, "start the run command: "+err.Error())
		}
		reply.Ran = &runtimecontract.Ran{ExitCode: command.ProcessState.ExitCode(), Stdout: string(stdout), Stderr: string(stderr)}
	}
	return http.StatusOK, reply
}

func (s *supervisor) empty() error {
	s.written, s.unpacked, s.installed = nil, "", nil
	return errors.Join(os.RemoveAll(s.root), os.MkdirAll(s.root, 0o700))
}

func (s *supervisor) decode(name string, value any) error {
	data, err := os.ReadFile(filepath.Join(s.root, name))
	if err != nil {
		return err
	}
	return protocol.DecodeJSONStrict(data, value)
}

// Install and prepare use the same commands in a live reload and a clean build.
func (s *supervisor) install(ctx context.Context, next runtimecontract.Install, output io.Writer, env ...string) error {
	if reflect.DeepEqual(s.installed, &next) {
		return nil
	}
	s.installed = nil
	if err := s.run(ctx, next.Commands, output, env...); err != nil {
		return err
	}
	s.installed = &next
	return nil
}

func (s *supervisor) run(ctx context.Context, commands [][]string, output io.Writer, env ...string) error {
	for _, argv := range commands {
		command := s.command(ctx, argv, env...)
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			return err
		}
	}
	return nil
}

func (s *supervisor) command(ctx context.Context, argv []string, env ...string) *exec.Cmd {
	if len(argv) == 0 {
		return &exec.Cmd{Err: errors.New("the plan has an empty command")}
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir, command.Env = s.root, append(append(os.Environ(), "PWD="+s.root), env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	return command
}

func (s *supervisor) stop() {
	if c := s.app.Swap(nil); c != nil {
		c.stop()
	}
}

type child struct {
	command *exec.Cmd
	address string
	proxy   http.Handler
	done    chan struct{}
}

func (s *supervisor) start(ctx context.Context, argv []string, output string) (int, runtimecontract.BuildReply) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return failure(http.StatusInternalServerError, err.Error())
	}
	address := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	command := s.command(context.Background(), argv, "HOST=127.0.0.1", "PORT="+strconv.Itoa(address.Port))
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return failure(http.StatusInternalServerError, "start application: "+err.Error())
	}
	target := &url.URL{Scheme: "http", Host: address.String()}
	c := &child{command: command, address: address.String(), done: make(chan struct{}), proxy: &httputil.ReverseProxy{
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
	published := make(chan struct{})
	defer close(published)
	go func() {
		_ = command.Wait()
		close(c.done)
		<-published
		if s.app.CompareAndSwap(c, nil) {
			fmt.Fprintln(os.Stderr, "application exited:", c.command.ProcessState)
		}
	}()
	status, problem := c.ready(ctx)
	if status != http.StatusOK {
		c.stop()
		return failure(status, problem)
	}
	s.app.Store(c)
	return status, runtimecontract.BuildReply{ReloadReply: runtimecontract.ReloadReply{Output: output}}
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

const tailBytes = 4 << 10

// tail keeps the end of what was written to it: at most tailBytes, from a line's start once it has dropped any.
type tail []byte

func (t *tail) Write(p []byte) (int, error) {
	*t = append(*t, p...)
	if extra := len(*t) - tailBytes; extra > 0 {
		*t = (*t)[extra:]
		if _, rest, found := bytes.Cut(*t, []byte("\n")); found {
			*t = rest
		}
	}
	return len(p), nil
}
