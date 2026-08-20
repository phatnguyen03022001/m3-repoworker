package process

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
)

func localSpec(cwd string, args ...string) ProcessSpec {
	return ProcessSpec{
		Execution:   security.CompiledExecution{Backend: "test-local", Executable: "/bin/sh", Arguments: args, CWD: cwd},
		Environment: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"},
		Timeout:     time.Second,
	}
}

func TestSupervisedProcessCapturesStreamsWithMonotonicCursor(t *testing.T) {
	supervisor, err := NewLocalForTests(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalForTests() error = %v", err)
	}
	spec := localSpec(t.TempDir(), "-c", "printf 'out'; printf 'err' >&2")
	process, err := supervisor.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	outcome, err := process.Wait(context.Background())
	if err != nil || outcome.ExitCode != 0 || outcome.TimedOut {
		t.Fatalf("Wait() = %#v, %v", outcome, err)
	}
	chunks, err := process.Read(0, 10)
	if err != nil || len(chunks) != 2 {
		t.Fatalf("Read() = %#v, %v", chunks, err)
	}
	if chunks[0].Cursor != 1 || chunks[1].Cursor != 2 || chunks[0].Stream == chunks[1].Stream || !strings.Contains(chunks[0].Data+chunks[1].Data, "out") || !strings.Contains(chunks[0].Data+chunks[1].Data, "err") {
		t.Fatalf("chunks = %#v", chunks)
	}
	resumed, err := process.Read(chunks[0].Cursor, 10)
	if err != nil || len(resumed) != 1 || resumed[0].Cursor != chunks[1].Cursor {
		t.Fatalf("resumed Read() = %#v, %v", resumed, err)
	}
}

func TestSupervisedProcessSpillsBoundedMemoryDurably(t *testing.T) {
	spillRoot := t.TempDir()
	supervisor, err := NewLocalForTests(spillRoot)
	if err != nil {
		t.Fatalf("NewLocalForTests() error = %v", err)
	}
	spec := localSpec(t.TempDir(), "-c", "i=0; while [ $i -lt 100 ]; do printf '0123456789'; i=$((i+1)); done")
	spec.MemoryBytes = 32
	spec.ChunkBytes = 8
	process, err := supervisor.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	outcome, err := process.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	chunks, err := process.Read(0, 1024)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("Read(spill) = %#v, %v", chunks, err)
	}
	if len(chunks) == 0 || outcome.SpillPath == "" {
		t.Fatalf("spill outcome = %#v", outcome)
	}
	info, err := os.Stat(outcome.SpillPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("spill file = %v, %v", info.Mode().Perm(), err)
	}
}

func TestTimeoutAndCancellationKillProcessGroup(t *testing.T) {
	supervisor, err := NewLocalForTests(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalForTests() error = %v", err)
	}
	spec := localSpec(t.TempDir(), "-c", "sleep 30 & wait")
	spec.Timeout = 100 * time.Millisecond
	process, err := supervisor.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start(timeout) error = %v", err)
	}
	outcome, err := process.Wait(context.Background())
	if err != nil || !outcome.TimedOut {
		t.Fatalf("timeout Wait() = %#v, %v", outcome, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process, err = supervisor.Start(ctx, localSpec(t.TempDir(), "-c", "sleep 30 & wait"))
	if err != nil {
		t.Fatalf("Start(cancel) error = %v", err)
	}
	cancel()
	outcome, err = process.Wait(context.Background())
	if err != nil || !outcome.Canceled {
		t.Fatalf("cancel Wait() = %#v, %v", outcome, err)
	}
}

func TestPTYOnlyForInteractiveProcess(t *testing.T) {
	supervisor, err := NewLocalForTests(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalForTests() error = %v", err)
	}
	spec := localSpec(t.TempDir(), "-c", "printf 'pty'")
	spec.PTY = true
	if _, err := supervisor.Start(context.Background(), spec); !errors.Is(err, ErrRejected) {
		t.Fatalf("non-interactive PTY error = %v", err)
	}
	spec.Interactive = true
	process, err := supervisor.Start(context.Background(), spec)
	if err != nil {
		if errors.Is(err, ErrRejected) {
			t.Skipf("PTY unavailable in this host sandbox: %v", err)
		}
		t.Fatalf("interactive PTY Start() error = %v", err)
	}
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatalf("PTY Wait() error = %v", err)
	}
	chunks, err := process.Read(0, 10)
	if err != nil || len(chunks) != 1 || chunks[0].Stream != StreamPTY || chunks[0].Data != "pty" {
		t.Fatalf("PTY chunks = %#v, %v", chunks, err)
	}
}

func TestSignalAndEnvironmentValidation(t *testing.T) {
	if ValidateSpec(ProcessSpec{Execution: security.CompiledExecution{Backend: "test-local", Executable: "/bin/echo", CWD: "/tmp"}, Environment: []string{"GITHUB_TOKEN=secret"}}) == nil {
		t.Fatal("credential environment accepted")
	}
	supervisor, err := NewLocalForTests(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalForTests() error = %v", err)
	}
	spec := localSpec(t.TempDir(), "-c", "trap 'exit 0' TERM; sleep 30")
	process, err := supervisor.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start(signal) error = %v", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	if outcome, err := process.Wait(context.Background()); err != nil || outcome.TimedOut {
		t.Fatalf("signal Wait() = %#v, %v", outcome, err)
	}
}

func TestProcessRejectsInvalidCWDAndEnvironment(t *testing.T) {
	if ValidateSpec(localSpec("/tmp/../tmp", "-c", "true")) == nil {
		t.Fatal("unclean cwd accepted")
	}
	if ValidateSpec(ProcessSpec{Execution: security.CompiledExecution{Backend: "test-local", Executable: "/bin/echo", CWD: "/tmp"}, Environment: []string{"BAD\x00=value"}}) == nil {
		t.Fatal("NUL environment accepted")
	}
	if !strings.Contains(string(StreamStdout), "stdout") {
		t.Fatal("stream constants malformed")
	}
}
