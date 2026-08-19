package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/repo"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
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

type ApplyPatchInput struct {
	Patch string `json:"patch" jsonschema:"strict single-file unified diff with a/ and b/ headers"`
}

type ApplyPatchOutput struct {
	Path     string `json:"path"`
	Modified bool   `json:"modified"`
}

type TaskCreateInput struct {
	NextAction string `json:"next_action,omitempty" jsonschema:"optional safe handoff note for the next development step"`
}

type TaskLookupInput struct {
	TaskID string `json:"task_id" jsonschema:"RepoWorker-generated task identifier"`
}

type taskManager interface {
	Create(context.Context, string) (taskstate.State, error)
	Status(context.Context, string) (taskstate.State, error)
	Resume(context.Context, string) (taskstate.State, error)
}

var errRequestRejected = errors.New("request rejected")

func boolPtr(v bool) *bool { return &v }

func newServer(repoRoot, stateRoot string) (*mcp.Server, error) {
	workspace, err := repo.New(repoRoot)
	if err != nil {
		return nil, errRequestRejected
	}
	tasks, err := taskstate.New(repoRoot, stateRoot)
	if err != nil {
		return nil, errRequestRejected
	}
	return newServerForComponents(workspace, tasks), nil
}

func newServerForComponents(workspace *repo.Workspace, tasks taskManager) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "m3-repoworker",
			Version: "0.2.0",
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
		func(_ context.Context, _ *mcp.CallToolRequest, input ApplyPatchInput) (*mcp.CallToolResult, ApplyPatchOutput, error) {
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
			Name:        "task_create",
			Title:       "Create development task",
			Description: "Create persistent RepoWorker handoff state bound to the configured repository, current branch, and HEAD.",
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
			Description: "Resume a persisted task only on its bound repository and branch, refreshing HEAD and forcing RED if it moved.",
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
	server, err := newServer(repoRoot, stateRoot)
	if err != nil {
		return errRequestRejected
	}
	err = server.Run(ctx, transport)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
