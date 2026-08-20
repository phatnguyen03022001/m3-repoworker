package process

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tienphat/m3-repoworker/internal/security"
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

const containerPath = "/workspace/node_modules/.bin:/workspace/.venv/bin:/workspace/bin:/usr/local/go/bin:/go/bin:/usr/local/cargo/bin:/root/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func (s ContainerStarter) Start(ctx context.Context, spec ProcessSpec) (Running, error) {
	if ctx == nil || s.Resolve == nil || spec.Execution.Backend != "apple-container" {
		return nil, ErrRejected
	}
	if !sameEnvironment(spec.Environment, spec.Execution.Environment) {
		return nil, ErrRejected
	}
	if err := security.ValidateUserEnvironment(spec.Environment, 64, 16<<10); err != nil {
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
	args := []string{"exec", "--workdir", spec.Execution.CWD}
	for _, value := range containerBaselineEnvironment() {
		args = append(args, "--env", value)
	}
	for _, value := range spec.Environment {
		// Only explicit key=value pairs are passed. In particular, never pass a
		// bare key because Apple container would inherit it from the host CLI.
		args = append(args, "--env", value)
	}
	args = append(args, containerID, spec.Execution.Executable)
	args = append(args, containerShellArguments(spec.Execution.Executable, spec.Execution.Arguments)...)
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

func containerBaselineEnvironment() []string {
	return []string{
		"PATH=" + containerPath,
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"GOPATH=/go",
		"GOMODCACHE=/go/pkg/mod",
		"GOCACHE=/tmp/go-build",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
}

// Login shells may reset PATH from /etc/profile after the container runtime
// has applied --env. Re-establish the deterministic candidate-only PATH inside
// the shell session without inspecting or restricting the caller's command.
func containerShellArguments(executable string, arguments []string) []string {
	result := append([]string(nil), arguments...)
	if len(result) >= 2 && result[0] == "-lc" && isDevelopmentShell(executable) {
		result[1] = "PATH='" + containerPath + "'; export PATH; " + result[1]
	}
	return result
}

func isDevelopmentShell(executable string) bool {
	base := strings.ToLower(filepath.Base(executable))
	return base == "sh" || base == "bash" || base == "zsh"
}

func sameEnvironment(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
