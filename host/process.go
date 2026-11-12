package host

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/holon-sh/protocol"
)

type application struct {
	Port     uint16 `json:"port"`
	Frontend string `json:"frontend,omitempty"`
}

// process is one child of the supervisor. Its directory is the image's application or an overlay.
type process struct {
	directory string
	command   *exec.Cmd
	cancel    context.CancelFunc
	ready     chan error
	done      chan struct{}
	built     chan struct{}
	err       error
}

// stop sends SIGTERM, escalates after the command's WaitDelay, and waits.
func (p *process) stop() {
	p.cancel()
	<-p.done
}

func Run(ctx context.Context, command []string) error {
	port := 3000
	if value, present := os.LookupEnv("PORT"); present {
		port, _ = strconv.Atoi(value)
		if !protocol.ValidPort(port) {
			return errors.New("invalid application port")
		}
	}
	h, err := newHost()
	if err != nil {
		return err
	}
	if h.image, err = os.Getwd(); err != nil {
		return err
	}
	directory := h.image
	var record string
	if record, h.development = os.LookupEnv(overlayFileEnv); h.development {
		// A writable volume outlives the container, and overlays from before this boot are of no use.
		if err := errors.Join(os.RemoveAll(h.overlays), os.MkdirAll(h.overlays, 0o700)); err != nil {
			return err
		}
		boot, _ := os.ReadFile(record)
		if directory, err = h.bootOverlay(ctx, boot); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	private, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	servers := make(chan error, 2)
	public := &http.Server{
		Handler: h, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	var opening sync.Once
	listen := func() (err error) {
		opening.Do(func() {
			var listener net.Listener
			if listener, err = new(net.ListenConfig).Listen(ctx, "tcp", ":"+strconv.Itoa(port)); err == nil {
				go func() { servers <- public.Serve(listener) }()
			}
		})
		return err
	}
	prefix := "/" + rand.Text()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"/ready", func(w http.ResponseWriter, r *http.Request) {
		var app application
		data, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if err != nil || protocol.DecodeJSONNonNull(data, &app) != nil || app.Port == 0 {
			http.Error(w, "Invalid application startup", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		starting := h.starting
		h.starting = nil
		h.mu.Unlock()
		if starting == nil {
			http.Error(w, "Application already started", http.StatusConflict)
			return
		}
		handler, err := frontend(app.Frontend, starting.directory)
		if err != nil {
			http.Error(w, "Invalid application frontend", http.StatusBadRequest)
			starting.ready <- err
			return
		}
		h.connect(app.Port, handler, starting)
		if err = listen(); err != nil {
			http.Error(w, "Application listener unavailable", http.StatusServiceUnavailable)
			starting.ready <- err
			return
		}
		starting.ready <- nil
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(prefix+"/peer/{peer}", h.peer)
	mux.HandleFunc("GET "+prefix+"/context/{scope}", h.context)
	broker := &http.Server{
		Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() { servers <- broker.Serve(private) }()
	brokerURL := "http://" + private.Addr().String() + prefix
	environment := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, protocol.RuntimeConfigEnv+"=")
	})
	for name := range h.peers {
		environment = append(environment, protocol.DependencyEnvName(name)+"="+brokerURL+"/peer/"+url.PathEscape(name))
	}
	environment = append(environment, "HOLON_HOST_URL="+brokerURL)
	start := func(directory string, page bool) (*process, error) {
		childCtx, cancelChild := context.WithCancel(ctx)
		child := exec.CommandContext(childCtx, command[0], command[1:]...)
		child.Dir = directory
		child.Env = append(slices.Clone(environment), "PWD="+directory)
		if directory != h.image && srcLayout(directory) {
			child.Env = append(child.Env, "PYTHONPATH="+filepath.Join(directory, "src")+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))
		}
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.Cancel = func() error { return child.Process.Signal(syscall.SIGTERM) }
		child.WaitDelay = 5 * time.Second
		p := &process{
			directory: directory, command: child, cancel: cancelChild,
			ready: make(chan error, 1), done: make(chan struct{}), built: make(chan struct{}),
		}
		h.mu.Lock()
		h.starting = p
		h.mu.Unlock()
		if err := child.Start(); err != nil {
			cancelChild()
			return nil, fmt.Errorf("start application: %w", err)
		}
		go func() {
			p.err = child.Wait()
			cancelChild()
			<-p.built
			close(p.done)
		}()
		go func() {
			if page && directory != h.image {
				build(childCtx, directory)
			}
			close(p.built)
		}()
		return p, nil
	}
	var current *process
	if directory != "" {
		started, failure := awaitReady(ctx, start, directory, true, nil)
		if failure != nil && !h.development {
			return errors.Join(failure, ctx.Err())
		}
		if failure != nil {
			fmt.Fprintln(os.Stderr, failure.Message, failure.Exit)
			_ = os.RemoveAll(directory)
		}
		current = started
	}
	if current == nil {
		if err := listen(); err != nil {
			return err
		}
	}
	defer func() {
		_ = public.Close()
		cancel()
		if current != nil {
			<-current.done
		}
	}()
	for {
		var exited <-chan struct{}
		if current != nil {
			exited = current.done
		}
		select {
		case <-exited:
			return errors.Join(errors.New("application exited"), current.err, ctx.Err())
		case err := <-servers:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case job := <-h.reloads:
			next, failure, err := h.restart(ctx, start, current, job)
			job.reply <- failure
			if err != nil {
				// Let the reply reach the caller before the pod restarts.
				shutdown, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				_ = public.Shutdown(shutdown)
				return err
			}
			current = next
		}
	}
}

// restart replaces the child with one running from job's directory, or restores the previous directory's child if
// the new one does not become ready. An error means neither is running.
func (h *host) restart(
	ctx context.Context, start func(string, bool) (*process, error), current *process, job *reloadJob,
) (*process, *reloadFailure, error) {
	job.restartedAt = time.Now()
	h.disconnect()
	if current != nil {
		current.stop()
	}
	next, failure := awaitReady(ctx, start, job.directory, job.page, job.gone)
	if failure == nil {
		job.readyAt = time.Now()
		if current != nil {
			_ = os.RemoveAll(current.directory)
		}
		return next, nil, nil
	}
	_ = os.RemoveAll(job.directory)
	if current == nil {
		return nil, failure, nil
	}
	// The previous source started before, so it gets as long as a first boot.
	previous, restored := awaitReady(ctx, start, current.directory, true, nil)
	if restored != nil {
		return nil, failure, fmt.Errorf("restore application after failed reload: %w", restored)
	}
	return previous, failure, nil
}

// awaitReady starts a child and returns it once it has posted ready; if it will not, it is not left running. A nil
// timeout waits as long as the child runs.
func awaitReady(
	ctx context.Context, start func(string, bool) (*process, error), directory string, page bool, timeout <-chan struct{},
) (*process, *reloadFailure) {
	p, err := start(directory, page)
	if err != nil {
		return nil, &reloadFailure{status: http.StatusInternalServerError, Message: err.Error()}
	}
	select {
	case err := <-p.ready:
		if err == nil {
			return p, nil
		}
		p.stop()
		return nil, &reloadFailure{status: http.StatusInternalServerError, Message: "application rejected: " + err.Error()}
	case <-p.done:
		return nil, &reloadFailure{
			status: http.StatusInternalServerError, Message: "application exited before ready", Exit: p.command.ProcessState.String(),
		}
	case <-timeout:
		p.stop()
		return nil, &reloadFailure{status: http.StatusGatewayTimeout, Message: "application did not become ready"}
	case <-ctx.Done():
		p.stop()
		return nil, &reloadFailure{status: http.StatusServiceUnavailable, Message: "runtime stopping"}
	}
}
