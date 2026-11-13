package host

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// process owns a command and its descendants. done closes only after the leader exits and its group is killed.
type process struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func startProcess(command *exec.Cmd) (*process, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return nil, err
	}
	p := &process{command: command, done: make(chan struct{})}
	go func() {
		p.err = command.Wait()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		close(p.done)
	}()
	return p, nil
}

func (p *process) wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		p.kill()
		return ctx.Err()
	}
}

func (p *process) kill() {
	select {
	case <-p.done:
		return
	default:
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// stop gives the group five seconds to end before killing it.
func (p *process) stop() {
	select {
	case <-p.done:
		return
	default:
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		p.kill()
	}
}
