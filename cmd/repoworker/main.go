package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/repo"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
	"golang.org/x/sys/unix"
)

type StatusInput struct{}

type StatusOutput struct {
	Status string `json:"status"`
}

type RepoReadInput struct {
	Path string `json:"path" jsonschema:"repository-relative file path to read"`
}

type RepoReadOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type RepoSearchInput struct {
	Query string `json:"query" jsonschema:"literal UTF-8 text to search for"`
	Path  string `json:"path,omitempty" jsonschema:"optional repository-relative file or directory scope"`
}

type RepoSearchOutput struct {
	Matches   []repo.Match `json:"matches"`
	Truncated bool         `json:"truncated"`
}

type RepoSnapshotInput struct{}

type ApplyPatchInput struct {
	Patch string `json:"patch" jsonschema:"strict single-file unified diff with a/ and b/ headers"`
}

type ApplyPatchOutput struct {
	Path     string `json:"path"`
	Modified bool   `json:"modified"`
}

type CreateFileInput struct {
	Path    string `json:"path" jsonschema:"repository-relative path for a new UTF-8 text file"`
	Content string `json:"content" jsonschema:"UTF-8 text content for the new file"`
}

type CreateFileOutput struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

type DeleteFileInput struct {
	Path string `json:"path" jsonschema:"repository-relative existing UTF-8 text file to delete"`
}

type DeleteFileOutput struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted"`
}

type RepoVerifyInput struct {
	Check string `json:"check" jsonschema:"verification preset: fmt, test, test-race, vet, mcp-integration, or verify"`
}

type RepoVerifyOutput struct {
	Check    string `json:"check"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

type GoModTidyInput struct{}

type GoModTidyOutput struct {
	Completed bool `json:"completed"`
	ExitCode  int  `json:"exit_code"`
	TimedOut  bool `json:"timed_out"`
}

type TaskCreateInput struct {
	NextAction string `json:"next_action,omitempty" jsonschema:"optional safe handoff note for the next development step"`
}

type TaskLookupInput struct {
	TaskID string `json:"task_id" jsonschema:"RepoWorker-generated task identifier"`
}

func requireMain(ctx context.Context, tasks taskstate.StateStore) error {
	if ctx == nil || tasks == nil {
		return taskstate.ErrRejected
	}
	return tasks.RequireMain(ctx)
}

var errRequestRejected = errors.New("request rejected")

func boolPtr(v bool) *bool { return &v }

type fixedCommandOutcome struct {
	exitCode int
	timedOut bool
}

func verificationPreset(check string) (string, time.Duration, bool) {
	switch check {
	case "fmt":
		return "fmt-check", 2 * time.Minute, true
	case "test":
		return "test", 5 * time.Minute, true
	case "test-race":
		return "test-race", 10 * time.Minute, true
	case "vet":
		return "vet", 5 * time.Minute, true
	case "mcp-integration":
		return "mcp-integration", 5 * time.Minute, true
	case "verify":
		return "verify", 15 * time.Minute, true
	default:
		return "", 0, false
	}
}

func resolveFixedExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil || !filepath.IsAbs(path) {
		return "", errRequestRejected
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) {
		return "", errRequestRejected
	}
	return path, nil
}

func fixedExecutionEnv(network bool) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GOCACHE=/dev/fd/3/.cache/go-build",
		"GOMODCACHE=/dev/fd/3/.cache/go-mod",
	}
	if network {
		env = append(env,
			"GOPROXY=https://proxy.golang.org",
			"GOSUMDB=sum.golang.org",
			"GOPRIVATE=",
			"GONOPROXY=none",
			"GONOSUMDB=",
		)
	} else {
		env = append(env, "GOPROXY=off")
	}
	return env
}

func runFixedCommand(ctx context.Context, root *os.File, executable string, args []string, env []string, timeout time.Duration) (fixedCommandOutcome, error) {
	if ctx == nil || root == nil || executable == "" || timeout <= 0 {
		return fixedCommandOutcome{}, errRequestRejected
	}
	path, err := resolveFixedExecutable(executable)
	if err != nil {
		return fixedCommandOutcome{}, errRequestRejected
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.Command(path, args...)
	command.ExtraFiles = []*os.File{root}
	command.Env = env
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fixedCommandOutcome{}, errRequestRejected
	}

	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()
	select {
	case err := <-wait:
		if err == nil {
			return fixedCommandOutcome{exitCode: 0}, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fixedCommandOutcome{exitCode: exitErr.ExitCode()}, nil
		}
		return fixedCommandOutcome{}, errRequestRejected
	case <-runCtx.Done():
		if command.Process != nil {
			_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		}
		<-wait
		if ctx.Err() != nil {
			return fixedCommandOutcome{}, errRequestRejected
		}
		return fixedCommandOutcome{exitCode: -1, timedOut: true}, nil
	}
}

func newServer(repoRoot, stateRoot string) (*mcp.Server, *repo.Workspace, error) {
	workspace, err := repo.New(repoRoot)
	if err != nil {
		return nil, nil, errRequestRejected
	}
	tasks, err := taskstate.New(workspace.StartupPath(), workspace.RootIdentity(), stateRoot)
	if err != nil {
		_ = workspace.Close()
		return nil, nil, errRequestRejected
	}
	if err := tasks.RequireMain(context.Background()); err != nil {
		_ = workspace.Close()
		return nil, nil, errRequestRejected
	}
	return newServerForComponents(workspace, tasks), workspace, nil
}

func newServerForComponents(workspace *repo.Workspace, tasks taskstate.StateStore) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "m3-repoworker",
			Version: "0.3.0",
		},
		nil,
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_status",
			Title:       "Repo status",
			Description: "Return the health status of the local M3 RepoWorker.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(context.Context, *mcp.CallToolRequest, StatusInput) (*mcp.CallToolResult, StatusOutput, error) {
			return nil, StatusOutput{Status: "ok"}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_read",
			Title:       "Read repository file",
			Description: "Read one permitted UTF-8 text file below the configured repository root.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(_ context.Context, _ *mcp.CallToolRequest, input RepoReadInput) (*mcp.CallToolResult, RepoReadOutput, error) {
			path, content, err := workspace.Read(input.Path)
			if err != nil {
				return nil, RepoReadOutput{}, safeToolError(err)
			}
			return nil, RepoReadOutput{Path: path, Content: content}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_search",
			Title:       "Search repository files",
			Description: "Perform a bounded literal search of permitted UTF-8 text files below the configured repository root.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input RepoSearchInput) (*mcp.CallToolResult, RepoSearchOutput, error) {
			result, err := workspace.Search(ctx, input.Query, input.Path)
			if err != nil {
				return nil, RepoSearchOutput{}, safeToolError(err)
			}
			return nil, RepoSearchOutput{Matches: result.Matches, Truncated: result.Truncated}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_snapshot",
			Title:       "Snapshot repository",
			Description: "Return a deterministic FD-relative manifest of permitted repository files.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ RepoSnapshotInput) (*mcp.CallToolResult, repo.SnapshotManifest, error) {
			manifest, err := workspace.Snapshot(ctx)
			if err != nil {
				return nil, repo.SnapshotManifest{}, safeToolError(err)
			}
			return nil, manifest, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "apply_patch",
			Title:       "Apply repository patch",
			Description: "Atomically apply one exact-context, single-file unified diff to a permitted repository text file.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  false,
				DestructiveHint: boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input ApplyPatchInput) (*mcp.CallToolResult, ApplyPatchOutput, error) {
			if err := requireMain(ctx, tasks); err != nil {
				return nil, ApplyPatchOutput{}, safeToolError(err)
			}
			path, err := workspace.ApplyPatch(input.Patch)
			if err != nil {
				return nil, ApplyPatchOutput{}, safeToolError(err)
			}
			return nil, ApplyPatchOutput{Path: path, Modified: true}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "create_file",
			Title:       "Create repository file",
			Description: "Create one new permitted UTF-8 text file without overwriting an existing path.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  false,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input CreateFileInput) (*mcp.CallToolResult, CreateFileOutput, error) {
			if err := requireMain(ctx, tasks); err != nil {
				return nil, CreateFileOutput{}, safeToolError(err)
			}
			path, err := workspace.CreateFile(input.Path, input.Content)
			if err != nil {
				return nil, CreateFileOutput{}, safeToolError(err)
			}
			return nil, CreateFileOutput{Path: path, Created: true}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "delete_file",
			Title:       "Delete repository file",
			Description: "Delete one existing permitted regular file without following symlinks.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  false,
				DestructiveHint: boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
			if err := requireMain(ctx, tasks); err != nil {
				return nil, DeleteFileOutput{}, safeToolError(err)
			}
			path, err := workspace.DeleteFile(input.Path)
			if err != nil {
				return nil, DeleteFileOutput{}, safeToolError(err)
			}
			return nil, DeleteFileOutput{Path: path, Deleted: true}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_verify",
			Title:       "Verify repository",
			Description: "Run one hard-coded repository verification preset with bounded execution.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input RepoVerifyInput) (*mcp.CallToolResult, RepoVerifyOutput, error) {
			if err := requireMain(ctx, tasks); err != nil {
				return nil, RepoVerifyOutput{}, safeToolError(err)
			}
			target, timeout, ok := verificationPreset(input.Check)
			if !ok {
				return nil, RepoVerifyOutput{}, errRequestRejected
			}
			root, err := workspace.DuplicateRoot()
			if err != nil {
				return nil, RepoVerifyOutput{}, safeToolError(err)
			}
			defer root.Close()
			outcome, err := runFixedCommand(ctx, root, "make", []string{"-C", "/dev/fd/3", target}, fixedExecutionEnv(false), timeout)
			if err != nil {
				return nil, RepoVerifyOutput{}, safeToolError(err)
			}
			return nil, RepoVerifyOutput{
				Check: input.Check, Passed: outcome.exitCode == 0 && !outcome.timedOut,
				ExitCode: outcome.exitCode, TimedOut: outcome.timedOut,
			}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "repo_go_mod_tidy",
			Title:       "Tidy Go modules",
			Description: "Run fixed go mod tidy against the opened repository root with registry-only module network access.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(true),
				OpenWorldHint:   boolPtr(true),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ GoModTidyInput) (*mcp.CallToolResult, GoModTidyOutput, error) {
			if err := requireMain(ctx, tasks); err != nil {
				return nil, GoModTidyOutput{}, safeToolError(err)
			}
			root, err := workspace.DuplicateRoot()
			if err != nil {
				return nil, GoModTidyOutput{}, safeToolError(err)
			}
			defer root.Close()
			outcome, err := runFixedCommand(
				ctx, root, "go", []string{"-C", "/dev/fd/3", "mod", "tidy"},
				fixedExecutionEnv(true), 10*time.Minute,
			)
			if err != nil {
				return nil, GoModTidyOutput{}, safeToolError(err)
			}
			return nil, GoModTidyOutput{
				Completed: outcome.exitCode == 0 && !outcome.timedOut,
				ExitCode:  outcome.exitCode, TimedOut: outcome.timedOut,
			}, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "task_create",
			Title:       "Create development task",
			Description: "Create persistent RepoWorker handoff state bound to the configured repository, main branch, and HEAD.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  false,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input TaskCreateInput) (*mcp.CallToolResult, taskstate.State, error) {
			state, err := tasks.Create(ctx, input.NextAction)
			if err != nil {
				return nil, taskstate.State{}, safeToolError(err)
			}
			return nil, state, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "task_status",
			Title:       "Read development task",
			Description: "Return persisted handoff state for one RepoWorker-generated task identifier.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input TaskLookupInput) (*mcp.CallToolResult, taskstate.State, error) {
			state, err := tasks.Status(ctx, input.TaskID)
			if err != nil {
				return nil, taskstate.State{}, safeToolError(err)
			}
			return nil, state, nil
		},
	)

	mcp.AddTool(
		server,
		&mcp.Tool{
			Name:        "task_resume",
			Title:       "Resume development task",
			Description: "Resume a persisted task only on its bound repository and main branch, refreshing HEAD and forcing RED if it moved.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				IdempotentHint:  true,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, input TaskLookupInput) (*mcp.CallToolResult, taskstate.State, error) {
			state, err := tasks.Resume(ctx, input.TaskID)
			if err != nil {
				return nil, taskstate.State{}, safeToolError(err)
			}
			return nil, state, nil
		},
	)

	return server
}

func safeToolError(error) error {
	return errRequestRejected
}

func main() {
	flags := flag.NewFlagSet("repoworker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoRoot := flags.String("repo-root", "", "absolute repository root")
	stateRoot := flags.String("state-dir", "", "absolute persistent task state directory outside the repository")
	if err := flags.Parse(os.Args[1:]); err != nil {
		log.Fatal("invalid startup configuration")
	}
	resolvedStateRoot := *stateRoot
	if resolvedStateRoot == "" {
		var err error
		resolvedStateRoot, err = taskstate.DefaultStateDir()
		if err != nil {
			log.Fatal("invalid startup configuration")
		}
	}
	if err := run(context.Background(), &mcp.StdioTransport{}, *repoRoot, resolvedStateRoot); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, transport mcp.Transport, repoRoot, stateRoot string) error {
	server, workspace, err := newServer(repoRoot, stateRoot)
	if err != nil {
		return errRequestRejected
	}
	defer func() {
		_ = workspace.Close()
	}()
	err = server.Run(ctx, transport)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
