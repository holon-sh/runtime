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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/holon-sh/protocol"
)

type application struct {
	Port     uint16 `json:"port"`
	Frontend string `json:"frontend,omitempty"`
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
	prefix := "/" + rand.Text()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"/ready", func(w http.ResponseWriter, r *http.Request) {
		var app application
		data, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if err != nil || protocol.DecodeJSONNonNull(data, &app) != nil || app.Port == 0 {
			http.Error(w, "Invalid application startup", http.StatusBadRequest)
			return
		}
		h.frontend, err = frontend(app.Frontend)
		if err != nil {
			http.Error(w, "Invalid application frontend", http.StatusBadRequest)
			servers <- err
			return
		}
		h.connect(app.Port)
		listener, err := new(net.ListenConfig).Listen(ctx, "tcp", ":"+strconv.Itoa(port))
		if err != nil {
			http.Error(w, "Application listener unavailable", http.StatusServiceUnavailable)
			servers <- err
			return
		}
		go func() { servers <- public.Serve(listener) }()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(prefix+"/peer/{peer}", h.peer)
	mux.HandleFunc("GET "+prefix+"/context/{scope}", h.context)
	broker := &http.Server{
		Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() { servers <- broker.Serve(private) }()
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	brokerURL := "http://" + private.Addr().String() + prefix
	child.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, protocol.RuntimeConfigEnv+"=")
	})
	for name := range h.peers {
		child.Env = append(child.Env, protocol.DependencyEnvName(name)+"="+brokerURL+"/peer/"+url.PathEscape(name))
	}
	child.Env = append(child.Env, "HOLON_HOST_URL="+brokerURL)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Cancel = func() error { return child.Process.Signal(syscall.SIGTERM) }
	child.WaitDelay = 5 * time.Second
	if err := child.Start(); err != nil {
		return fmt.Errorf("start application: %w", err)
	}
	done := make(chan struct{})
	var childErr error
	go func() {
		childErr = child.Wait()
		close(done)
	}()
	defer func() {
		_ = public.Close()
		cancel()
		<-done
	}()
	select {
	case <-done:
		return errors.Join(errors.New("application exited"), childErr, ctx.Err())
	case err := <-servers:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
