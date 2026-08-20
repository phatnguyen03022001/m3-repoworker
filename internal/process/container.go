package process

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// RuntimeResolver maps the opaque runtime identity exposed to MCP to the
// backend's external container identity. The resolver is supplied by the
// composition root; clients never provide a container name directly.
type RuntimeResolver func(context.Context, string) (string, error)

// ContainerStarter executes only through the selected runtime backend. It is
// deliberately separate from commandStarter, which is test-only and never
// wired into the production MCP server.
type ContainerStarter struct {
	Resolve RuntimeResolver
	Binary  string
}

func (s ContainerStarter) Start(ctx context.Context, spec ProcessSpec) (Running, error) {
	if ctx == nil || s.Resolve == nil || spec.Execution.Backend != "apple-container" || spec.Environment != nil {
		return nil, ErrRejected
	}
	if !safeWorkspaceCWD(spec.Execution.CWD) {
		return nil, ErrRejected
	}
	if spec.RuntimeID == "" || spec.WorkspaceGeneration == "" || spec.LeaseGeneration == 0 {
		return nil, ErrRejected
	}
	containerID, err := s.Resolve(ctx, spec.RuntimeID)
	if err != nil || containerID == "" {
		return nil, ErrRejected
	}
	binary := s.Binary
	if binary == "" {
		binary = "container"
	}
	if filepath.Base(binary) != "container" || !filepath.IsAbs(binary) {
		return nil, ErrRejected
	}
	args := []string{"exec", "--workdir", spec.Execution.CWD, containerID, spec.Execution.Executable}
	args = append(args, spec.Execution.Arguments...)
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/opt/homebrew/bin", "LC_ALL=C"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	running := &containerRunning{cmd: command}
	if spec.PTY {
		return nil, ErrRejected
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, ErrRejected
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, ErrRejected
	}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, ErrRejected
	}
	running.stdout, running.stderr = stdout, stderr
	return running, nil
}

type containerRunning struct {
	cmd          *exec.Cmd
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	processGroup bool
}

func (r *containerRunning) Wait() error           { return r.cmd.Wait() }
func (r *containerRunning) Stdout() io.ReadCloser { return r.stdout }
func (r *containerRunning) Stderr() io.ReadCloser { return r.stderr }
func (r *containerRunning) PTY() io.ReadCloser    { return nil }
func (r *containerRunning) PID() int {
	if r.cmd == nil || r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}
func (r *containerRunning) Signal(signal os.Signal) error {
	if r.cmd == nil || r.cmd.Process == nil {
		return ErrRejected
	}
	if value, ok := signal.(syscall.Signal); ok && r.processGroup {
		return unix.Kill(-r.cmd.Process.Pid, value)
	}
	return r.cmd.Process.Signal(signal)
}
func (r *containerRunning) Close() error {
	if r.stdout != nil {
		_ = r.stdout.Close()
	}
	if r.stderr != nil {
		_ = r.stderr.Close()
	}
	return nil
}

func safeWorkspaceCWD(path string) bool {
	return path == "/workspace" || (len(path) > len("/workspace/") && path[:len("/workspace/")] == "/workspace/" && !containsDotDot(path))
}

func containsDotDot(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

var _ Starter = ContainerStarter{}
