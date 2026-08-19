package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRepoStatusTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := newServer(root)
	if err != nil {
		t.Fatalf("newServer() error = %v", err)
	}
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "repoworker-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 4 {
		t.Fatalf("tool count = %d, want 4", len(tools.Tools))
	}

	toolByName := make(map[string]*mcp.Tool)
	for _, tool := range tools.Tools {
		toolByName[tool.Name] = tool
	}
	for _, name := range []string{"repo_status", "repo_read", "repo_search"} {
		tool := toolByName[name]
		if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s annotations = %#v, want read-only, idempotent, closed-world, and non-destructive", name, tool)
		}
	}
	patchTool := toolByName["apply_patch"]
	if patchTool == nil || patchTool.Annotations == nil || patchTool.Annotations.ReadOnlyHint || patchTool.Annotations.IdempotentHint || patchTool.Annotations.DestructiveHint == nil || !*patchTool.Annotations.DestructiveHint || patchTool.Annotations.OpenWorldHint == nil || *patchTool.Annotations.OpenWorldHint {
		t.Errorf("apply_patch annotations = %#v, want mutating, non-idempotent, closed-world, and destructive", patchTool)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "repo_status"})
	if err != nil {
		t.Fatalf("call repo_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("repo_status returned an error: %#v", result.Content)
	}

	output, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured output type = %T, want map[string]any", result.StructuredContent)
	}
	if output["status"] != "ok" {
		t.Errorf("status = %#v, want ok", output["status"])
	}
}

func TestRunTreatsEOFAsCleanShutdown(t *testing.T) {
	t.Parallel()

	transport := &mcp.IOTransport{
		Reader: io.NopCloser(strings.NewReader("")),
		Writer: nopWriteCloser{Writer: io.Discard},
	}
	if err := run(context.Background(), transport, t.TempDir()); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

func TestMCPRepositoryToolsAndSanitizedRejection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "main.go"), "package main\nold value\n")
	writeFile(t, filepath.Join(root, ".env"), "must not be exposed\n")

	client := connectClient(t, root)
	ctx := context.Background()

	readResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "repo_read", Arguments: map[string]any{"path": "src/main.go"}})
	if err != nil || readResult.IsError {
		t.Fatalf("repo_read result = %#v, error = %v", readResult, err)
	}
	readOutput := structuredMap(t, readResult)
	if readOutput["path"] != "src/main.go" || readOutput["content"] != "package main\nold value\n" {
		t.Errorf("repo_read output = %#v", readOutput)
	}

	searchResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "repo_search", Arguments: map[string]any{"query": "old value"}})
	if err != nil || searchResult.IsError {
		t.Fatalf("repo_search result = %#v, error = %v", searchResult, err)
	}
	searchOutput := structuredMap(t, searchResult)
	matches, ok := searchOutput["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("repo_search matches = %#v, want one match", searchOutput["matches"])
	}

	patch := "--- a/src/main.go\n+++ b/src/main.go\n@@ -1,2 +1,2 @@\n package main\n-old value\n+new value\n"
	patchResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "apply_patch", Arguments: map[string]any{"patch": patch}})
	if err != nil || patchResult.IsError {
		t.Fatalf("apply_patch result = %#v, error = %v", patchResult, err)
	}
	patchOutput := structuredMap(t, patchResult)
	if patchOutput["path"] != "src/main.go" || patchOutput["modified"] != true {
		t.Errorf("apply_patch output = %#v", patchOutput)
	}

	updatedResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "repo_read", Arguments: map[string]any{"path": "src/main.go"}})
	if err != nil || updatedResult.IsError {
		t.Fatalf("repo_read after patch result = %#v, error = %v", updatedResult, err)
	}
	if got := structuredMap(t, updatedResult)["content"]; got != "package main\nnew value\n" {
		t.Errorf("updated content = %#v", got)
	}

	for _, request := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "repo_read", arguments: map[string]any{"path": ".env"}},
		{name: "repo_search", arguments: map[string]any{"query": "value", "path": "../outside"}},
		{name: "apply_patch", arguments: map[string]any{"patch": "--- a/.env\n+++ b/.env\n@@ -1 +1 @@\n-old\n+new\n"}},
	} {
		rejected, err := client.CallTool(ctx, &mcp.CallToolParams{Name: request.name, Arguments: request.arguments})
		if err != nil {
			t.Fatalf("%s rejected call protocol error = %v", request.name, err)
		}
		if !rejected.IsError || len(rejected.Content) != 1 {
			t.Fatalf("%s rejected result = %#v, want one tool error", request.name, rejected)
		}
		text, ok := rejected.Content[0].(*mcp.TextContent)
		if !ok || text.Text != "request rejected" || strings.Contains(text.Text, root) || strings.Contains(text.Text, ".env") || strings.Contains(text.Text, "outside") {
			t.Errorf("%s unsanitized error content = %#v", request.name, rejected.Content)
		}
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func connectClient(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server, err := newServer(root)
	if err != nil {
		t.Fatalf("newServer() error = %v", err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "repoworker-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return clientSession
}

func structuredMap(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	output, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured output type = %T, want map[string]any", result.StructuredContent)
	}
	return output
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
