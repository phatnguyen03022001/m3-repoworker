package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if preset, ok := InternalRequest(); ok {
		os.Exit(RunInternal(preset))
	}
	os.Exit(m.Run())
}

func TestVerificationPresetsAndEnvironmentsAreClosed(t *testing.T) {
	for _, check := range []string{"fmt", "test", "test-race", "vet", "mcp-integration", "verify"} {
		preset, ok := VerificationPreset(check)
		if !ok || preset != Preset(check) {
			t.Fatalf("VerificationPreset(%q) = %q, %v", check, preset, ok)
		}
		environment := strings.Join(commandEnvironment(preset), "\n")
		if !strings.Contains(environment, "GOPROXY=off") {
			t.Fatalf("%s environment enables network: %q", check, environment)
		}
	}
	if _, ok := VerificationPreset("go-mod-tidy"); ok {
		t.Fatal("maintenance preset exposed through verification selector")
	}
	if _, ok := VerificationPreset("shell"); ok {
		t.Fatal("unknown preset accepted")
	}

	t.Setenv("GITHUB_TOKEN", "must-not-propagate")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/must-not-propagate")
	maintenance := strings.Join(commandEnvironment(PresetGoModTidy), "\n")
	if !strings.Contains(maintenance, "GOPROXY=https://proxy.golang.org") ||
		!strings.Contains(maintenance, "GONOPROXY=none") ||
		strings.Contains(maintenance, "must-not-propagate") ||
		strings.Contains(maintenance, "GITHUB_TOKEN=") ||
		strings.Contains(maintenance, "SSH_AUTH_SOCK=") {
		t.Fatalf("maintenance environment is not registry-only: %q", maintenance)
	}
	if strings.Contains(maintenance, InternalPresetEnvironment+"=") {
		t.Fatal("internal runner marker leaked to fixed command")
	}
}

func TestRunUsesInheritedRootAfterStartupPathReplacement(t *testing.T) {
	parent := t.TempDir()
	startupPath := filepath.Join(parent, "repo")
	if err := os.Mkdir(startupPath, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	writeRunnerFile(t, filepath.Join(startupPath, "Makefile"), "fmt-check:\n\t@printf 'opened\\n' > marker\n")
	root, err := os.Open(startupPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	movedPath := filepath.Join(parent, "repo-moved")
	if err := os.Rename(startupPath, movedPath); err != nil {
		t.Fatalf("rename root: %v", err)
	}
	if err := os.Mkdir(startupPath, 0o700); err != nil {
		t.Fatalf("mkdir replacement: %v", err)
	}
	writeRunnerFile(t, filepath.Join(startupPath, "Makefile"), "fmt-check:\n\t@printf 'replacement\\n' > marker\n")

	outcome, err := Run(context.Background(), root, PresetFmt, []string{startupPath, movedPath})
	if err != nil || outcome.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", outcome, err)
	}
	if got := readRunnerFile(t, filepath.Join(movedPath, "marker")); got != "opened\n" {
		t.Fatalf("opened root marker = %q", got)
	}
	if _, err := os.Stat(filepath.Join(startupPath, "marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement root executed: %v", err)
	}
	if _, err := root.Stat(); err != nil {
		t.Fatalf("Run closed caller root FD: %v", err)
	}
}

func TestRunRejectsUnknownPresetAndCacheSymlink(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()
	if _, err := Run(context.Background(), root, Preset("shell"), nil); !errors.Is(err, ErrRejected) {
		t.Fatalf("Run(unknown) error = %v, want ErrRejected", err)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(rootPath, ".cache")); err != nil {
		t.Fatalf("symlink cache: %v", err)
	}
	writeRunnerFile(t, filepath.Join(rootPath, "Makefile"), "fmt-check:\n\t@true\n")
	outcome, err := Run(context.Background(), root, PresetFmt, []string{rootPath, outside})
	if err != nil {
		t.Fatalf("Run(cache symlink) error = %v", err)
	}
	if outcome.ExitCode != 125 || outcome.FailureStage != StageStart {
		t.Fatalf("Run(cache symlink) = %#v, want fail-closed start failure", outcome)
	}
}

func TestRunPreservesChildExitAndSanitizesDiagnostic(t *testing.T) {
	rootPath := t.TempDir()
	writeRunnerFile(t, filepath.Join(rootPath, "go.mod"), "not a go module\n")
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	outcome, err := Run(context.Background(), root, PresetGoModTidy, []string{rootPath})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.ExitCode != 1 || outcome.FailureStage != StageExecution || outcome.TimedOut {
		t.Fatalf("Run() = %#v, want exact go exit 1", outcome)
	}
	if strings.Contains(outcome.Diagnostic, rootPath) || strings.Contains(outcome.Diagnostic, "/dev/fd/") {
		t.Fatalf("diagnostic leaked a path: %q", outcome.Diagnostic)
	}
}

func TestRunCapsCombinedOutput(t *testing.T) {
	rootPath := t.TempDir()
	payload := strings.Repeat("x", CaptureLimit+4096)
	makefile := "fmt-check:\n\t@printf '%s' '" + payload + "'; exit 7\n"
	writeRunnerFile(t, filepath.Join(rootPath, "Makefile"), makefile)
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	outcome, err := Run(context.Background(), root, PresetFmt, []string{rootPath})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.ExitCode != 2 || outcome.FailureStage != StageExecution || !outcome.Truncated {
		t.Fatalf("Run() = %#v, want bounded make failure", outcome)
	}
	if len(outcome.Diagnostic) > DiagnosticLimit || !utf8.ValidString(outcome.Diagnostic) {
		t.Fatalf("diagnostic length/encoding = %d/%v", len(outcome.Diagnostic), utf8.ValidString(outcome.Diagnostic))
	}
}

func TestSanitizeDiagnosticRedactsAndSuppresses(t *testing.T) {
	repository := filepath.Join(string(filepath.Separator), "private", "repo")
	diagnostic, truncated, suppressed := sanitizeDiagnostic(
		[]byte("failed in "+repository+" and /dev/fd/3/source.go\n"),
		[]string{repository},
	)
	if truncated || suppressed || diagnostic == "" {
		t.Fatalf("sanitizeDiagnostic() = %q, %v, %v", diagnostic, truncated, suppressed)
	}
	if strings.Contains(diagnostic, repository) || strings.Contains(diagnostic, "/dev/fd/") {
		t.Fatalf("diagnostic leaked path: %q", diagnostic)
	}
	for name, input := range map[string][]byte{
		"secret":       []byte("Authorization: Bearer hidden\n"),
		"private-key":  []byte("-----BEGIN PRIVATE KEY-----\n"),
		"invalid-utf8": {0xff, 0xfe},
		"nul":          {'o', 'k', 0, 'x'},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, suppressed := sanitizeDiagnostic(input, nil)
			if !suppressed || got != "" {
				t.Fatalf("sanitizeDiagnostic(%s) = %q, %v, want suppression", name, got, suppressed)
			}
		})
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	rootPath := t.TempDir()
	writeRunnerFile(t, filepath.Join(rootPath, "Makefile"), "fmt-check:\n\t@sleep 30 & echo $$! > child.pid; wait\n")
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	outcome, err := runWithTimeout(context.Background(), root, PresetFmt, []string{rootPath}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("runWithTimeout() error = %v", err)
	}
	if !outcome.TimedOut || outcome.FailureStage != StageTimeout {
		t.Fatalf("runWithTimeout() = %#v, want timeout", outcome)
	}
	assertRecordedProcessGone(t, filepath.Join(rootPath, "child.pid"))
}

func TestRunCancellationKillsProcessGroup(t *testing.T) {
	rootPath := t.TempDir()
	writeRunnerFile(t, filepath.Join(rootPath, "Makefile"), "fmt-check:\n\t@echo ready > ready; sleep 30 & echo $$! > child.pid; wait\n")
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runWithTimeout(ctx, root, PresetFmt, []string{rootPath}, time.Minute)
		result <- err
	}()
	waitForRunnerFile(t, filepath.Join(rootPath, "ready"))
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("runWithTimeout(cancel) error = %v, want context.Canceled", err)
	}
	assertRecordedProcessGone(t, filepath.Join(rootPath, "child.pid"))
}

func assertRecordedProcessGone(t *testing.T, path string) {
	t.Helper()
	data := readRunnerFile(t, path)
	pid, err := strconv.Atoi(strings.TrimSpace(data))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still exists: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRunnerFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeRunnerFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", filepath.Base(path), err)
	}
}

func readRunnerFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", filepath.Base(path), err)
	}
	return string(content)
}
