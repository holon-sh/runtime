package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	mcpbridge "github.com/holon-sh/mcp-bridge/http"

	"github.com/holon-sh/runtime/host"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("holon-mcp-runtime", flag.ContinueOnError)
	transport := flags.String("transport", "stdio", "MCP transport")
	origin := flags.String("origin", "", "trusted public origin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command := flags.Args()
	if len(command) == 0 {
		return errors.New("missing MCP executable")
	}
	if *transport == "hosted-http" {
		return host.Run(ctx, command)
	}
	if *transport != "stdio" {
		return errors.New("unsupported MCP transport")
	}
	protection := http.NewCrossOriginProtection()
	if err := protection.AddTrustedOrigin(*origin); err != nil {
		return err
	}
	bridge, err := mcpbridge.Start(ctx, func() *exec.Cmd {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stderr = os.Stderr
		return cmd
	}, mcpbridge.Options{ProtocolVersion: mcpbridge.ProtocolVersion})
	if err != nil {
		return errors.New("could not start MCP server")
	}
	defer bridge.Close()
	mux := http.NewServeMux()
	mux.Handle("/mcp", protection.Handler(bridge))
	server := &http.Server{Addr: ":3000", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	exited := make(chan error, 2)
	go func() { exited <- server.ListenAndServe() }()
	go func() { exited <- bridge.Wait() }()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-exited:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return err
}
