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

var errRequestRejected = errors.New("request rejected")

func boolPtr(v bool) *bool { return &v }

func newServer(repoRoot string) (*mcp.Server, error) {
	workspace, err := repo.New(repoRoot)
	if err != nil {
		return nil, errRequestRejected
	}
	return newServerForWorkspace(workspace), nil
}

func newServerForWorkspace(workspace *repo.Workspace) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "m3-repoworker",
			Version: "0.1.0",
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
		func(_ context.Context, _ *mcp.CallToolRequest, input RepoSearchInput) (*mcp.CallToolResult, RepoSearchOutput, error) {
			result, err := workspace.Search(input.Query, input.Path)
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

	return server
}

func safeToolError(error) error {
	return errRequestRejected
}

func main() {
	flags := flag.NewFlagSet("repoworker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repoRoot := flags.String("repo-root", "", "absolute repository root")
	if err := flags.Parse(os.Args[1:]); err != nil {
		log.Fatal("invalid startup configuration")
	}
	if err := run(context.Background(), &mcp.StdioTransport{}, *repoRoot); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, transport mcp.Transport, repoRoot string) error {
	server, err := newServer(repoRoot)
	if err != nil {
		return errRequestRejected
	}
	err = server.Run(ctx, transport)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
