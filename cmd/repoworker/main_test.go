package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/repo"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
)

func TestRepoStatusTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workspace, err := repo.New(root)
	if err != nil {
		t.Fatalf("repo.New() error = %v", err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := newServerForComponents(workspace, &fakeTaskManager{})
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
	if len(tools.Tools) != 12 {
		t.Fatalf("tool count = %d, want 12", len(tools.Tools))
	}

	toolByName := make(map[string]*mcp.Tool)
	for _, tool := range tools.Tools {
		toolByName[tool.Name] = tool
	}
	for _, name := range []string{"repo_status", "repo_read", "repo_search", "repo_snapshot", "task_status"} {
		tool := toolByName[name]
		if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s annotations = %#v, want read-only, idempotent, closed-world, and non-destructive", name, tool)
		}
	}
	patchTool := toolByName["apply_patch"]
	if patchTool == nil || patchTool.Annotations == nil || patchTool.Annotations.ReadOnlyHint || patchTool.Annotations.IdempotentHint || patchTool.Annotations.DestructiveHint == nil || !*patchTool.Annotations.DestructiveHint || patchTool.Annotations.OpenWorldHint == nil || *patchTool.Annotations.OpenWorldHint {
		t.Errorf("apply_patch annotations = %#v, want mutating, non-idempotent, closed-world, and destructive", patchTool)
	}
	fileCreateTool := toolByName["create_file"]
	if fileCreateTool == nil || fileCreateTool.Annotations == nil || fileCreateTool.Annotations.ReadOnlyHint || fileCreateTool.Annotations.IdempotentHint || fileCreateTool.Annotations.DestructiveHint == nil || *fileCreateTool.Annotations.DestructiveHint || fileCreateTool.Annotations.OpenWorldHint == nil || *fileCreateTool.Annotations.OpenWorldHint {
		t.Errorf("create_file annotations = %#v, want mutating, non-idempotent, non-destructive, closed-world", fileCreateTool)
	}
	deleteTool := toolByName["delete_file"]
	if deleteTool == nil || deleteTool.Annotations == nil || deleteTool.Annotations.ReadOnlyHint || deleteTool.Annotations.IdempotentHint || deleteTool.Annotations.DestructiveHint == nil || !*deleteTool.Annotations.DestructiveHint || deleteTool.Annotations.OpenWorldHint == nil || *deleteTool.Annotations.OpenWorldHint {
		t.Errorf("delete_file annotations = %#v, want mutating, non-idempotent, destructive, closed-world", deleteTool)
	}
	verifyTool := toolByName["repo_verify"]
	if verifyTool == nil || verifyTool.Annotations == nil || verifyTool.Annotations.ReadOnlyHint || !verifyTool.Annotations.IdempotentHint || verifyTool.Annotations.DestructiveHint == nil || !*verifyTool.Annotations.DestructiveHint || verifyTool.Annotations.OpenWorldHint == nil || *verifyTool.Annotations.OpenWorldHint {
		t.Errorf("repo_verify annotations = %#v, want mutating, idempotent, destructive, closed-world", verifyTool)
	}
	tidyTool := toolByName["repo_go_mod_tidy"]
	if tidyTool == nil || tidyTool.Annotations == nil || tidyTool.Annotations.ReadOnlyHint || !tidyTool.Annotations.IdempotentHint || tidyTool.Annotations.DestructiveHint == nil || !*tidyTool.Annotations.DestructiveHint || tidyTool.Annotations.OpenWorldHint == nil || !*tidyTool.Annotations.OpenWorldHint {
		t.Errorf("repo_go_mod_tidy annotations = %#v, want mutating, idempotent, destructive, open-world", tidyTool)
	}
	createTool := toolByName["task_create"]
	if createTool == nil || createTool.Annotations == nil || createTool.Annotations.ReadOnlyHint || createTool.Annotations.IdempotentHint || createTool.Annotations.DestructiveHint == nil || *createTool.Annotations.DestructiveHint || createTool.Annotations.OpenWorldHint == nil || *createTool.Annotations.OpenWorldHint {
		t.Errorf("task_create annotations = %#v, want mutating, non-idempotent, non-destructive, closed-world", createTool)
	}
	resumeTool := toolByName["task_resume"]
	if resumeTool == nil || resumeTool.Annotations == nil || resumeTool.Annotations.ReadOnlyHint || !resumeTool.Annotations.IdempotentHint || resumeTool.Annotations.DestructiveHint == nil || *resumeTool.Annotations.DestructiveHint || resumeTool.Annotations.OpenWorldHint == nil || *resumeTool.Annotations.OpenWorldHint {
		t.Errorf("task_resume annotations = %#v, want mutating, idempotent, non-destructive, closed-world", resumeTool)
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
	if err := run(context.Background(), transport, t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

func TestMCPRepositoryToolsAndSanitizedRejection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "main.go"), "package main\nold value\n")
	writeFile(t, filepath.Join(root, ".env"), "must not be exposed\n")

	client := connectClient(t, root, &fakeTaskManager{})
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

	snapshotResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "repo_snapshot"})
	if err != nil || snapshotResult.IsError {
		t.Fatalf("repo_snapshot result = %#v, error = %v", snapshotResult, err)
	}
	snapshotOutput := structuredMap(t, snapshotResult)
	if snapshotID, ok := snapshotOutput["snapshot_id"].(string); !ok || len(snapshotID) != 64 {
		t.Fatalf("repo_snapshot id = %#v, want 64 hex chars", snapshotOutput["snapshot_id"])
	}

	createResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "create_file", Arguments: map[string]any{"path": "src/new.go", "content": "package main\n"}})
	if err != nil || createResult.IsError {
		t.Fatalf("create_file result = %#v, error = %v", createResult, err)
	}
	createOutput := structuredMap(t, createResult)
	if createOutput["path"] != "src/new.go" || createOutput["created"] != true {
		t.Fatalf("create_file output = %#v", createOutput)
	}

	deleteResult, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "delete_file", Arguments: map[string]any{"path": "src/new.go"}})
	if err != nil || deleteResult.IsError {
		t.Fatalf("delete_file result = %#v, error = %v", deleteResult, err)
	}
	deleteOutput := structuredMap(t, deleteResult)
	if deleteOutput["path"] != "src/new.go" || deleteOutput["deleted"] != true {
		t.Fatalf("delete_file output = %#v", deleteOutput)
	}
	if _, err := os.Stat(filepath.Join(root, "src", "new.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete_file left target present: %v", err)
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
		{name: "create_file", arguments: map[string]any{"path": ".env", "content": "hidden\n"}},
		{name: "delete_file", arguments: map[string]any{"path": ".env"}},
		{name: "repo_verify", arguments: map[string]any{"check": "shell"}},
		{name: "apply_patch", arguments: map[string]any{"patch": "--- a/.env\n+++ b/.env\n@@ -1 +1 @@\n-old\n+new\n"}},
	} {
		rejected, err := client.CallTool(ctx, &mcp.CallToolParams{Name: request.name, Arguments: request.arguments})
		if err != nil {
			t.Fatalf("%s rejected call protocol error = %v", request.name, err)
		}
		assertSanitizedToolError(t, request.name, rejected, root)
	}
}

func TestMCPTaskToolsAndSanitizedRejection(t *testing.T) {
	root := t.TempDir()
	tasks := &fakeTaskManager{state: taskstate.State{
		Version: 1, TaskID: "task_0123456789abcdef0123456789abcdef",
		RepoRootIdentity: strings.Repeat("a", 64), Branch: "main",
		BaseSHA: strings.Repeat("b", 40), CurrentHeadSHA: strings.Repeat("b", 40),
		VerificationState: "RED", FailedChecks: []string{}, NextAction: "continue work",
		CreatedAt: "2026-08-19T00:00:00Z", UpdatedAt: "2026-08-19T00:00:00Z",
	}}
	client := connectClient(t, root, tasks)
	ctx := context.Background()

	created, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "task_create", Arguments: map[string]any{"next_action": "continue work"}})
	if err != nil || created.IsError {
		t.Fatalf("task_create result = %#v, error = %v", created, err)
	}
	if got := structuredMap(t, created)["task_id"]; got != tasks.state.TaskID {
		t.Fatalf("task_create task_id = %#v", got)
	}
	if tasks.lastNextAction != "continue work" {
		t.Errorf("task_create next_action = %q", tasks.lastNextAction)
	}

	for _, name := range []string{"task_status", "task_resume"} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"task_id": tasks.state.TaskID}})
		if err != nil || result.IsError {
			t.Fatalf("%s result = %#v, error = %v", name, result, err)
		}
		if got := structuredMap(t, result)["branch"]; got != "main" {
			t.Errorf("%s branch = %#v", name, got)
		}
	}

	tasks.err = errors.New("sensitive internal failure /Users/example/.env")
	rejected, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "task_status", Arguments: map[string]any{"task_id": tasks.state.TaskID}})
	if err != nil {
		t.Fatalf("task_status protocol error = %v", err)
	}
	assertSanitizedToolError(t, "task_status", rejected, "/Users/example")
}

func TestVerificationPresetAndEnvironmentAreFixed(t *testing.T) {
	cases := map[string]string{
		"fmt":             "fmt-check",
		"test":            "test",
		"test-race":       "test-race",
		"vet":             "vet",
		"mcp-integration": "mcp-integration",
		"verify":          "verify",
	}
	for check, wantTarget := range cases {
		target, timeout, ok := verificationPreset(check)
		if !ok || target != wantTarget || timeout <= 0 {
			t.Fatalf("verificationPreset(%q) = (%q, %v, %v)", check, target, timeout, ok)
		}
	}
	if _, _, ok := verificationPreset("shell"); ok {
		t.Fatal("verificationPreset(shell) unexpectedly accepted arbitrary command")
	}

	offline := strings.Join(fixedExecutionEnv(false), "\n")
	if !strings.Contains(offline, "GOPROXY=off") || strings.Contains(offline, "GITHUB_TOKEN=") || strings.Contains(offline, "SSH_AUTH_SOCK=") {
		t.Fatalf("offline execution environment is not confined: %q", offline)
	}
	maintenance := strings.Join(fixedExecutionEnv(true), "\n")
	if !strings.Contains(maintenance, "GOPROXY=https://proxy.golang.org") || strings.Contains(maintenance, "GOPRIVATE="+os.Getenv("GOPRIVATE")) && os.Getenv("GOPRIVATE") != "" {
		t.Fatalf("maintenance execution environment is not fixed: %q", maintenance)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

type fakeTaskManager struct {
	state          taskstate.State
	err            error
	lastNextAction string
	lastTaskID     string
}

func (f *fakeTaskManager) Create(_ context.Context, nextAction string) (taskstate.State, error) {
	f.lastNextAction = nextAction
	return f.state, f.err
}

func (f *fakeTaskManager) Status(_ context.Context, taskID string) (taskstate.State, error) {
	f.lastTaskID = taskID
	return f.state, f.err
}

func (f *fakeTaskManager) Resume(_ context.Context, taskID string) (taskstate.State, error) {
	f.lastTaskID = taskID
	return f.state, f.err
}

func connectClient(t *testing.T, root string, tasks taskstate.StateStore) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	workspace, err := repo.New(root)
	if err != nil {
		t.Fatalf("repo.New() error = %v", err)
	}
	server := newServerForComponents(workspace, tasks)
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

func assertSanitizedToolError(t *testing.T, name string, result *mcp.CallToolResult, forbidden string) {
	t.Helper()
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("%s rejected result = %#v, want one tool error", name, result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "request rejected" || strings.Contains(text.Text, forbidden) || strings.Contains(text.Text, ".env") {
		t.Errorf("%s unsanitized error content = %#v", name, result.Content)
	}
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
