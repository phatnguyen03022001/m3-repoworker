// Package process provides supervised process handles independent of a
// runtime backend. MCP callers do not receive a host-process capability; a
// runtime backend supplies the same typed execution contract to this layer.
package process

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tienphat/m3-repoworker/internal/security"
	"golang.org/x/sys/unix"
)

const (
	DefaultChunkBytes  = 32 << 10
	DefaultMemoryBytes = 256 << 10
	DefaultTimeout     = 10 * time.Minute
)

var ErrRejected = errors.New("process request rejected")

type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	StreamPTY    Stream = "pty"
)

type Chunk struct {
	Cursor Cursor `json:"cursor"`
	Stream Stream `json:"stream"`
	Data   string `json:"data"`
}

type Cursor uint64

type ProcessSpec struct {
	Execution   security.CompiledExecution
	Environment []string
	Interactive bool
	PTY         bool
	Timeout     time.Duration
	MemoryBytes int
	ChunkBytes  int
}

type Outcome struct {
	ExitCode  int
	TimedOut  bool
	Canceled  bool
	Truncated bool
	SpillPath string
}

type Starter interface {
	Start(context.Context, ProcessSpec) (Running, error)
}

type Running interface {
	Wait() error
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	PTY() io.ReadCloser
	PID() int
	Signal(os.Signal) error
	Close() error
}

type Supervisor struct {
	starter   Starter
	spillRoot string
}

type Process struct {
	running Running
	cancel  context.CancelFunc
	done    chan struct{}

	mu          sync.Mutex
	next        Cursor
	memory      []Chunk
	memoryBytes int
	memoryLimit int
	chunkBytes  int
	spillPath   string
	spill       *os.File
	outcome     Outcome
	waitErr     error
	finished    bool
}

type commandStarter struct{}

// New returns a supervisor backed by a starter. Production runtime adapters
// provide a starter that executes inside Apple container or Lima; the local
// command starter is deliberately opt-in for process-layer tests/tools.
func New(starter Starter, spillRoot string) (*Supervisor, error) {
	if starter == nil || spillRoot == "" || !filepath.IsAbs(spillRoot) {
		return nil, ErrRejected
	}
	if err := os.MkdirAll(spillRoot, 0o700); err != nil || os.Chmod(spillRoot, 0o700) != nil {
		return nil, ErrRejected
	}
	return &Supervisor{starter: starter, spillRoot: spillRoot}, nil
}

// NewLocalForTests is not a host capability for MCP. It exists to exercise
// supervision semantics before M3.4 supplies a runtime backend.
func NewLocalForTests(spillRoot string) (*Supervisor, error) {
	return New(commandStarter{}, spillRoot)
}

func (s *Supervisor) Start(ctx context.Context, spec ProcessSpec) (*Process, error) {
	if ctx == nil || s == nil || s.starter == nil {
		return nil, ErrRejected
	}
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	running, err := s.starter.Start(runCtx, spec)
	if err != nil {
		cancel()
		return nil, ErrRejected
	}
	process := &Process{running: running, cancel: cancel, done: make(chan struct{}), memoryLimit: spec.MemoryBytes, chunkBytes: spec.ChunkBytes}
	if process.memoryLimit <= 0 {
		process.memoryLimit = DefaultMemoryBytes
	}
	if process.chunkBytes <= 0 || process.chunkBytes > DefaultChunkBytes {
		process.chunkBytes = DefaultChunkBytes
	}
	go process.supervise(runCtx, s.spillRoot, spec.PTY)
	return process, nil
}

func (p *Process) supervise(ctx context.Context, spillRoot string, usePTY bool) {
	var readers sync.WaitGroup
	read := func(stream Stream, closer io.ReadCloser) {
		if closer == nil {
			return
		}
		readers.Add(1)
		go func() {
			defer readers.Done()
			defer closer.Close()
			buffer := make([]byte, p.chunkBytes)
			for {
				n, err := closer.Read(buffer)
				if n > 0 {
					p.appendChunk(stream, string(buffer[:n]), spillRoot)
				}
				if err != nil {
					return
				}
			}
		}()
	}
	if usePTY {
		read(StreamPTY, p.running.PTY())
	} else {
		read(StreamStdout, p.running.Stdout())
		read(StreamStderr, p.running.Stderr())
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- p.running.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		p.killGroup()
		waitErr = <-waitDone
	}
	readers.Wait()
	p.mu.Lock()
	p.waitErr = waitErr
	p.finished = true
	p.outcome.ExitCode = exitCode(waitErr)
	p.outcome.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	p.outcome.Canceled = errors.Is(ctx.Err(), context.Canceled)
	p.outcome.SpillPath = p.spillPath
	p.mu.Unlock()
	p.cancel()
	close(p.done)
}

func (p *Process) appendChunk(stream Stream, data, spillRoot string) {
	if data == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	chunk := Chunk{Cursor: p.next, Stream: stream, Data: data}
	if p.spill != nil || p.memoryBytes+len(data) > p.memoryLimit {
		if p.spill == nil {
			path := filepath.Join(spillRoot, ".process-spill-"+randomSuffix()+".jsonl")
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				p.spill = file
				p.spillPath = path
				for _, old := range p.memory {
					_ = writeChunk(p.spill, old)
				}
				p.memory = nil
				p.memoryBytes = 0
			}
		}
		if p.spill != nil {
			_ = writeChunk(p.spill, chunk)
			_ = p.spill.Sync()
			return
		}
		p.outcome.Truncated = true
		return
	}
	p.memory = append(p.memory, chunk)
	p.memoryBytes += len(data)
}

func (p *Process) Wait(ctx context.Context) (Outcome, error) {
	if ctx == nil {
		return Outcome{}, ErrRejected
	}
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.outcome, nil
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

func (p *Process) Cancel() { p.cancel() }

func (p *Process) Signal(signal os.Signal) error {
	if p == nil || p.running == nil || signal == nil {
		return ErrRejected
	}
	return p.running.Signal(signal)
}

func (p *Process) Read(after Cursor, limit int) ([]Chunk, error) {
	if p == nil || limit <= 0 || limit > 1024 {
		return nil, ErrRejected
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Chunk, 0, limit)
	if p.spillPath != "" {
		file, err := os.Open(p.spillPath)
		if err != nil {
			return nil, ErrRejected
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() && len(result) < limit {
			var chunk Chunk
			if json.Unmarshal(scanner.Bytes(), &chunk) != nil {
				_ = file.Close()
				return nil, ErrRejected
			}
			if chunk.Cursor > after {
				result = append(result, chunk)
			}
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return nil, ErrRejected
		}
		_ = file.Close()
	}
	for _, chunk := range p.memory {
		if len(result) >= limit {
			break
		}
		if chunk.Cursor > after {
			result = append(result, chunk)
		}
	}
	return result, nil
}

func ValidateSpec(spec ProcessSpec) error {
	if spec.Execution.Executable == "" || spec.Execution.Backend == "" || !filepath.IsAbs(spec.Execution.CWD) || filepath.Clean(spec.Execution.CWD) != spec.Execution.CWD || strings.ContainsAny(spec.Execution.CWD, "\x00\r\n") {
		return ErrRejected
	}
	if spec.PTY && !spec.Interactive {
		return ErrRejected
	}
	if len(spec.Environment) > 128 {
		return ErrRejected
	}
	for _, value := range spec.Environment {
		if !validEnvironment(value) {
			return ErrRejected
		}
	}
	return nil
}

func validEnvironment(value string) bool {
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PRIVATE_KEY", "AUTH", "SSH_AUTH_SOCK"} {
		if strings.Contains(upper, marker) {
			return false
		}
	}
	return true
}

func (p *Process) killGroup() {
	if p.running == nil || p.running.PID() <= 0 {
		return
	}
	_ = unix.Kill(-p.running.PID(), unix.SIGTERM)
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = unix.Kill(-p.running.PID(), unix.SIGKILL)
	}
}

func writeChunk(file *os.File, chunk Chunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	return err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func randomSuffix() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

type localRunning struct {
	cmd    *exec.Cmd
	pty    *os.File
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (commandStarter) Start(ctx context.Context, spec ProcessSpec) (Running, error) {
	command := exec.Command(spec.Execution.Executable, spec.Execution.Arguments...)
	command.Dir = spec.Execution.CWD
	command.Env = append([]string(nil), spec.Environment...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	running := &localRunning{cmd: command}
	if spec.PTY {
		terminal, err := pty.Start(command)
		if err != nil {
			return nil, err
		}
		running.pty = terminal
	} else {
		stdout, err := command.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr, err := command.StderrPipe()
		if err != nil {
			_ = stdout.Close()
			return nil, err
		}
		if err := command.Start(); err != nil {
			_ = stdout.Close()
			_ = stderr.Close()
			return nil, err
		}
		running.stdout, running.stderr = stdout, stderr
	}
	return running, nil
}

func (r *localRunning) Wait() error           { return r.cmd.Wait() }
func (r *localRunning) Stdout() io.ReadCloser { return r.stdout }
func (r *localRunning) Stderr() io.ReadCloser { return r.stderr }
func (r *localRunning) PTY() io.ReadCloser    { return r.pty }
func (r *localRunning) PID() int {
	if r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}
func (r *localRunning) Signal(signal os.Signal) error {
	if r.cmd.Process == nil {
		return ErrRejected
	}
	if value, ok := signal.(syscall.Signal); ok {
		return unix.Kill(-r.cmd.Process.Pid, value)
	}
	return r.cmd.Process.Signal(signal)
}
func (r *localRunning) Close() error {
	if r.pty != nil {
		return r.pty.Close()
	}
	if r.stdout != nil {
		_ = r.stdout.Close()
	}
	if r.stderr != nil {
		_ = r.stderr.Close()
	}
	return nil
}
