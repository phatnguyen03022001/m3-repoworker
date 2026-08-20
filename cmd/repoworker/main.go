package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/controlplane"
	"github.com/tienphat/m3-repoworker/internal/operator"
	"github.com/tienphat/m3-repoworker/internal/repo"
	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
	"github.com/tienphat/m3-repoworker/internal/verify"
)

type StatusInput struct{}

type StatusOutput struct {
	Status string `json:"status"`
}

type RepoStatusOutput struct {
	Status       string `json:"status"`
	RepositoryID string `json:"repository_id"`
	PrincipalID  string `json:"principal_id"`
	SessionID    string `json:"session_id"`
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
	Check        string `json:"check"`
	Passed       bool   `json:"passed"`
	ExitCode     int    `json:"exit_code"`
	TimedOut     bool   `json:"timed_out"`
	FailureStage string `json:"failure_stage,omitempty"`
	Diagnostic   string `json:"diagnostic,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

type GoModTidyInput struct{}

type GoModTidyOutput struct {
	Completed    bool   `json:"completed"`
	ExitCode     int    `json:"exit_code"`
	TimedOut     bool   `json:"timed_out"`
	FailureStage string `json:"failure_stage,omitempty"`
	Diagnostic   string `json:"diagnostic,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
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

func newServer(repoRoot, stateRoot string) (*mcp.Server, *controlplane.Plane, error) {
	return newServerWithProvider(repoRoot, stateRoot, nil)
}

func newServerWithProvider(repoRoot, stateRoot string, provider security.PrincipalProvider) (*mcp.Server, *controlplane.Plane, error) {
	return newServerWithProviderAndAuthority(repoRoot, stateRoot, provider, nil)
}

func newServerWithProviderAndAuthority(repoRoot, stateRoot string, provider security.PrincipalProvider, authority security.OperatorAuthority) (*mcp.Server, *controlplane.Plane, error) {
	plane, err := controlplane.Open(context.Background(), controlplane.Config{RepositoryRoot: repoRoot, StateRoot: stateRoot, PrincipalProvider: provider, OperatorAuthority: authority})
	if err != nil {
		return nil, nil, errRequestRejected
	}
	return newControlPlaneServer(plane), plane, nil
}

func newServerForComponents(workspace *repo.Workspace, tasks taskstate.StateStore, stateRoot string) *mcp.Server {
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
			Name:        "repo_git_status",
			Title:       "Read Git status",
			Description: "Return typed read-only Git status bound to the opened repository authority.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(false)},
		},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ RepoSnapshotInput) (*mcp.CallToolResult, repo.GitStatus, error) {
			status, err := workspace.GitStatus(ctx)
			if err != nil {
				return nil, repo.GitStatus{}, safeToolError(err)
			}
			return nil, status, nil
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
			preset, ok := verify.VerificationPreset(input.Check)
			if !ok {
				return nil, RepoVerifyOutput{}, errRequestRejected
			}
			root, err := workspace.DuplicateRoot()
			if err != nil {
				return nil, RepoVerifyOutput{}, safeToolError(err)
			}
			defer root.Close()
			outcome, err := verify.Run(ctx, root, preset, []string{workspace.StartupPath(), stateRoot})
			if err != nil {
				return nil, RepoVerifyOutput{}, safeToolError(err)
			}
			return nil, RepoVerifyOutput{
				Check:        input.Check,
				Passed:       outcome.ExitCode == 0 && !outcome.TimedOut,
				ExitCode:     outcome.ExitCode,
				TimedOut:     outcome.TimedOut,
				FailureStage: string(outcome.FailureStage),
				Diagnostic:   outcome.Diagnostic,
				Truncated:    outcome.Truncated,
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
			outcome, err := verify.Run(ctx, root, verify.PresetGoModTidy, []string{workspace.StartupPath(), stateRoot})
			if err != nil {
				return nil, GoModTidyOutput{}, safeToolError(err)
			}
			return nil, GoModTidyOutput{
				Completed:    outcome.ExitCode == 0 && !outcome.TimedOut,
				ExitCode:     outcome.ExitCode,
				TimedOut:     outcome.TimedOut,
				FailureStage: string(outcome.FailureStage),
				Diagnostic:   outcome.Diagnostic,
				Truncated:    outcome.Truncated,
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

func safeToolError(err error) error {
	if errors.Is(err, taskstate.ErrMainOnly) {
		return taskstate.ErrMainOnly
	}
	return errRequestRejected
}

func main() {
	if preset, ok := verify.InternalRequest(); ok {
		os.Exit(verify.RunInternal(preset))
	}
	if len(os.Args) > 1 && os.Args[1] == "operator-approve" {
		if err := runOperatorApprove(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "operator request rejected")
			os.Exit(1)
		}
		return
	}
	flags := flag.NewFlagSet("repoworker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoRoot := flags.String("repo-root", "", "absolute repository root")
	stateRoot := flags.String("state-dir", "", "absolute persistent task state directory outside the repository")
	trustedPrincipal := flags.String("trusted-principal", "", "explicit principal for an already-authenticated local transport")
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
	if *trustedPrincipal == "" {
		log.Fatal("missing explicit transport authentication configuration")
	}
	provider, err := security.NewTrustedPrincipalProvider(*trustedPrincipal)
	if err != nil {
		log.Fatal("invalid transport authentication configuration")
	}
	if err := runWithProvider(context.Background(), &mcp.StdioTransport{}, *repoRoot, resolvedStateRoot, provider); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, transport mcp.Transport, repoRoot, stateRoot string) error {
	return runWithProvider(ctx, transport, repoRoot, stateRoot, nil)
}

func runWithProvider(ctx context.Context, transport mcp.Transport, repoRoot, stateRoot string, provider security.PrincipalProvider) error {
	return runWithProviderAndSocket(ctx, transport, repoRoot, stateRoot, provider, filepath.Join(stateRoot, "operator.sock"))
}

func runWithProviderAndSocket(ctx context.Context, transport mcp.Transport, repoRoot, stateRoot string, provider security.PrincipalProvider, socketPath string) error {
	transport = &replayMetadataTransport{delegate: transport}
	key, err := ensureOperatorKey(stateRoot)
	if err != nil {
		return errRequestRejected
	}
	operatorAuthority, err := security.NewAuthenticatedOperatorAuthority("local-operator")
	if err != nil {
		return errRequestRejected
	}
	server, plane, err := newServerWithProviderAndAuthority(repoRoot, stateRoot, provider, operatorAuthority)
	if err != nil {
		return errRequestRejected
	}
	operatorServer, err := operator.NewServer(socketPath, key, operatorAuthority.OperatorID, plane.IssueOperatorConfirmation)
	if err != nil {
		_ = plane.Close()
		return errRequestRejected
	}
	if err := operatorServer.Start(); err != nil {
		_ = plane.Close()
		return errRequestRejected
	}
	defer operatorServer.Close()
	defer func() {
		_ = plane.Close()
	}()
	err = server.Run(ctx, transport)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func ensureOperatorKey(stateRoot string) ([]byte, error) {
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, fmt.Errorf("invalid state root")
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return nil, errRequestRejected
	}
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		return nil, errRequestRejected
	}
	info, err := os.Stat(stateRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errRequestRejected
	}
	path := filepath.Join(stateRoot, "operator.key")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errRequestRejected
		}
		key, readErr := os.ReadFile(path)
		if readErr != nil || len(key) < 32 {
			return nil, errRequestRejected
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errRequestRejected
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, errRequestRejected
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errRequestRejected
	}
	_, writeErr := file.Write(key)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, errRequestRejected
	}
	return key, nil
}

func runOperatorApprove(args []string) error {
	flags := flag.NewFlagSet("operator-approve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("socket", "", "private RepoWorker operator socket")
	keyFile := flags.String("operator-key-file", "", "private operator key file")
	operatorID := flags.String("operator-id", "local-operator", "independent operator identity")
	class := flags.String("class", "destructive", "destructive or publication")
	ttl := flags.Duration("ttl", 10*time.Minute, "confirmation lifetime")
	action := flags.String("action", "", "exact approved action digest")
	operation := flags.String("operation", "", "typed operation shortcut: integrate")
	repositoryID := flags.String("repository-id", "", "repository identity")
	principalID := flags.String("principal-id", "", "autonomous principal identity")
	sessionID := flags.String("session-id", "", "authenticated control-plane session")
	generationID := flags.String("generation-id", "", "workspace generation")
	fencing := flags.Uint64("fencing-generation", 0, "workspace fencing generation")
	candidate := flags.String("candidate-snapshot", "", "candidate snapshot digest")
	plan := flags.String("plan-digest", "", "verification or operation plan digest")
	if err := flags.Parse(args); err != nil {
		return errRequestRejected
	}
	if *operation != "" && *operation != "integrate" {
		return errRequestRejected
	}
	if *action == "" && *operation == "integrate" && *plan != "" {
		digest := sha256.Sum256([]byte("repository.integrate:" + *plan))
		*action = hex.EncodeToString(digest[:])
	}
	if *action == "" || *repositoryID == "" || *principalID == "" || *sessionID == "" || *generationID == "" || *candidate == "" || *plan == "" || *fencing == 0 {
		return errRequestRejected
	}
	keyInfo, err := os.Lstat(*keyFile)
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o077 != 0 {
		return errRequestRejected
	}
	key, err := os.ReadFile(*keyFile)
	if err != nil || len(key) < 32 {
		return errRequestRejected
	}
	confirmationClass := security.ConfirmationClass(*class)
	if confirmationClass != security.ConfirmationDestructive && confirmationClass != security.ConfirmationPublication {
		return errRequestRejected
	}
	confirmation, err := operator.Approve(context.Background(), *socket, key, *operatorID, security.OperatorConfirmationRequest{Binding: security.ConfirmationBinding{Action: *action, RepositoryID: *repositoryID, PrincipalID: *principalID, SessionID: *sessionID, GenerationID: *generationID, FencingGeneration: *fencing, CandidateSnapshot: *candidate, PlanDigest: *plan}, Class: confirmationClass, TTL: *ttl})
	if err != nil {
		return errRequestRejected
	}
	_, err = fmt.Fprintln(os.Stdout, confirmation.Token)
	return err
}
