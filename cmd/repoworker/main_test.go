package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/repo"
	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
	"github.com/tienphat/m3-repoworker/internal/verify"
)

func TestMain(m *testing.M) {
	if preset, ok := verify.InternalRequest(); ok {
		os.Exit(verify.RunInternal(preset))
	}
	os.Exit(m.Run())
}

func TestRepoStatusTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workspace, err := repo.New(root)
	if err != nil {
		t.Fatalf("repo.New() error = %v", err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := newServerForComponents(workspace, &fakeTaskManager{}, "")
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
	if len(tools.Tools) != 13 {
		t.Fatalf("tool count = %d, want 13", len(tools.Tools))
	}

	toolByName := make(map[string]*mcp.Tool)
	for _, tool := range tools.Tools {
		toolByName[tool.Name] = tool
	}
	for _, name := range []string{"repo_status", "repo_read", "repo_search", "repo_snapshot", "repo_git_status", "task_status"} {
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

func TestProductionMCPToolSurface(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.invalid/fixture\n\ngo 1.26.6\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "RepoWorker Test"},
		{"config", "user.email", "repoworker@example.invalid"},
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		commandArgs := append([]string{"-C", root}, args...)
		if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	server, plane, err := newServerWithProvider(root, t.TempDir(), testPrincipalProvider(t))
	if err != nil {
		t.Fatalf("newServer() error = %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "surface-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	tools, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	toolByName := make(map[string]*mcp.Tool, len(tools.Tools))
	for _, tool := range tools.Tools {
		toolByName[tool.Name] = tool
	}
	expectedTools := []string{
		"repo_status", "repo_read", "repo_search", "repo_snapshot", "repo_git_status", "repo_verify",
		"workspace_create", "workspace_status", "workspace_discard", "workspace_integration_plan", "workspace_integrate",
		"runtime_create", "runtime_start", "runtime_stop", "runtime_status",
		"process_run", "process_read", "process_signal", "process_cancel", "process_wait",
		"verification_plan", "verification_run", "verification_status",
		"run_create", "run_event_append", "run_events",
		"loop_start", "loop_resume", "loop_status", "publication_plan", "publication_execute",
	}
	expected := make(map[string]struct{}, len(expectedTools))
	for _, name := range expectedTools {
		expected[name] = struct{}{}
		if toolByName[name] == nil {
			t.Errorf("production tool %q is missing", name)
		}
	}
	if len(tools.Tools) != len(expectedTools) {
		t.Fatalf("production tool count = %d, want exact %d", len(tools.Tools), len(expectedTools))
	}
	for _, tool := range tools.Tools {
		if _, ok := expected[tool.Name]; !ok {
			t.Errorf("unexpected production tool %q", tool.Name)
		}
	}
	for _, name := range []string{"apply_patch", "create_file", "delete_file", "host_exec", "shell", "confirmation_issue"} {
		if toolByName[name] != nil {
			t.Errorf("unsafe or maintenance-only tool %q is exposed in production", name)
		}
	}
	gitStatusTool := toolByName["repo_git_status"]
	if gitStatusTool == nil || gitStatusTool.Annotations == nil || !gitStatusTool.Annotations.ReadOnlyHint || !gitStatusTool.Annotations.IdempotentHint || gitStatusTool.Annotations.DestructiveHint == nil || *gitStatusTool.Annotations.DestructiveHint || gitStatusTool.Annotations.OpenWorldHint == nil || *gitStatusTool.Annotations.OpenWorldHint {
		t.Errorf("repo_git_status annotations = %#v, want read-only, idempotent, non-destructive, closed-world", gitStatusTool)
	}
	verifyTool := toolByName["repo_verify"]
	if verifyTool == nil || verifyTool.Annotations == nil || !verifyTool.Annotations.ReadOnlyHint || !verifyTool.Annotations.IdempotentHint || verifyTool.Annotations.DestructiveHint == nil || *verifyTool.Annotations.DestructiveHint || verifyTool.Annotations.OpenWorldHint == nil || *verifyTool.Annotations.OpenWorldHint {
		t.Errorf("repo_verify annotations = %#v, want read-only, idempotent, non-destructive, closed-world", verifyTool)
	}
	gitResult, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "repo_git_status"})
	if err != nil || gitResult.IsError {
		t.Fatalf("repo_git_status call = %#v, %v", gitResult, err)
	}
	gitOutput, ok := gitResult.StructuredContent.(map[string]any)
	if !ok || gitOutput["branch"] != "main" || gitOutput["dirty"] != false {
		t.Fatalf("repo_git_status output = %#v, want clean main status", gitResult.StructuredContent)
	}
}

func TestProductionServerRejectsMissingAuthenticationProvider(t *testing.T) {
	root := t.TempDir()
	if _, _, err := newServer(root, t.TempDir()); err == nil {
		t.Fatal("production server opened without an explicit authentication provider")
	}
}

func TestMCPMutatingRequestReplayRejectedAtRequestBoundary(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.invalid/replay\n\ngo 1.26.6\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "RepoWorker Test"},
		{"config", "user.email", "repoworker@example.invalid"},
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	server, plane, err := newServerWithProvider(root, t.TempDir(), testPrincipalProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "replay-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	request := &mcp.CallToolParams{Meta: mcp.Meta{security.MCPRequestIDMetaKey: "workspace-create-1", security.MCPRequestSequenceMetaKey: uint64(1)}, Name: "workspace_create", Arguments: map[string]any{"task_id": "replay-task"}}
	first, err := clientSession.CallTool(context.Background(), request)
	if err != nil || first.IsError {
		t.Fatalf("first mutating request = %#v, error = %v", first, err)
	}
	firstOutput := structuredMap(t, first)
	if firstOutput["generation_id"] == "" {
		t.Fatalf("first workspace output = %#v", firstOutput)
	}
	second, err := clientSession.CallTool(context.Background(), request)
	if err == nil && (second == nil || !second.IsError) {
		t.Fatalf("replayed mutating request = %#v, error = %v; want rejection", second, err)
	}
	if _, statusErr := plane.WorkspaceStatus(context.Background(), firstOutput["generation_id"].(string)); statusErr != nil {
		t.Fatalf("first workspace disappeared after replay rejection: %v", statusErr)
	}
}

func TestProductionRepoVerifyPresetsSequentially(t *testing.T) {
	if os.Getenv("REPOWORKER_RUN_PRESET_SEQUENCE") != "1" {
		t.Skip("set REPOWORKER_RUN_PRESET_SEQUENCE=1 for the explicit sequential repo_verify gate")
	}
	if os.Getenv(verify.InternalPresetEnvironment) != "" {
		t.Skip("fixed preset execution must not recursively invoke the sequential gate")
	}
	if testing.Short() {
		t.Skip("long verification sequence")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	server, plane, err := newServerWithProvider(root, t.TempDir(), testPrincipalProvider(t))
	if err != nil {
		t.Fatalf("newServer() error = %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "preset-sequence-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	for index, check := range []string{"fmt", "test", "test-race", "vet", "mcp-integration", "verify"} {
		result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Meta: mcp.Meta{security.MCPRequestIDMetaKey: fmt.Sprintf("preset-%s", check), security.MCPRequestSequenceMetaKey: uint64(index + 1)}, Name: "repo_verify", Arguments: map[string]any{"check": check}})
		if err != nil || result.IsError {
			t.Fatalf("repo_verify(%s) result = %#v, error = %v", check, result, err)
		}
		output := structuredMap(t, result)
		if output["passed"] != true {
			t.Fatalf("repo_verify(%s) output = %#v", check, output)
		}
	}
}

func TestRunTreatsEOFAsCleanShutdown(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "README.md"), "main\n")
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "RepoWorker Test"},
		{"config", "user.email", "repoworker@example.invalid"},
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		commandArgs := append([]string{"-C", root}, args...)
		if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	transport := &mcp.IOTransport{
		Reader: io.NopCloser(strings.NewReader("")),
		Writer: nopWriteCloser{Writer: io.Discard},
	}
	stateRoot := t.TempDir()
	socketRoot, err := os.MkdirTemp("/private/tmp", "rwop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	if err := runWithProviderAndSocket(context.Background(), transport, root, stateRoot, testPrincipalProvider(t), filepath.Join(socketRoot, "operator.sock")); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

func testPrincipalProvider(t *testing.T) security.PrincipalProvider {
	t.Helper()
	provider, err := security.NewTrustedPrincipalProvider("mcp-test-client")
	if err != nil {
		t.Fatal(err)
	}
	return provider
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

func TestMCPMutationsFailClosedWhenCheckoutLeavesMain(t *testing.T) {
	root := t.TempDir()
	tasks := &fakeTaskManager{mainErr: taskstate.ErrMainOnly}
	client := connectClient(t, root, tasks)
	ctx := context.Background()

	for _, request := range []struct {
		name      string
		arguments map[string]any
	}{
		{name: "apply_patch", arguments: map[string]any{"patch": "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n"}},
		{name: "create_file", arguments: map[string]any{"path": "new.txt", "content": "new\n"}},
		{name: "delete_file", arguments: map[string]any{"path": "file.txt"}},
		{name: "repo_verify", arguments: map[string]any{"check": "fmt"}},
		{name: "repo_go_mod_tidy", arguments: map[string]any{}},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: request.name, Arguments: request.arguments})
		if err != nil {
			t.Fatalf("%s protocol error = %v", request.name, err)
		}
		assertMainOnlyToolError(t, request.name, result)
	}
}

func TestMCPVerificationReturnsBoundedSanitizedDiagnostic(t *testing.T) {
	root := t.TempDir()
	makefile := "fmt-check:\n\t@printf 'failure in " + root + "\\n'; exit 7\n"
	writeFile(t, filepath.Join(root, "Makefile"), makefile)

	client := connectClient(t, root, &fakeTaskManager{})
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "repo_verify", Arguments: map[string]any{"check": "fmt"}})
	if err != nil || result.IsError {
		t.Fatalf("repo_verify result = %#v, error = %v", result, err)
	}
	output := structuredMap(t, result)
	if output["passed"] != false || output["exit_code"] != float64(2) {
		t.Fatalf("repo_verify outcome = %#v", output)
	}
	if output["failure_stage"] != "execution" {
		t.Fatalf("failure stage = %#v, want execution", output["failure_stage"])
	}
	diagnostic, ok := output["diagnostic"].(string)
	if !ok || diagnostic == "" {
		t.Fatalf("diagnostic = %#v, want bounded text", output["diagnostic"])
	}
	if len(diagnostic) > verify.DiagnosticLimit {
		t.Fatalf("diagnostic length = %d", len(diagnostic))
	}
	if strings.Contains(diagnostic, root) || strings.Contains(diagnostic, "/dev/fd/") {
		t.Fatalf("diagnostic leaked a path: %q", diagnostic)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

type fakeTaskManager struct {
	state          taskstate.State
	err            error
	mainErr        error
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

func (f *fakeTaskManager) RequireMain(context.Context) error {
	return f.mainErr
}

func connectClient(t *testing.T, root string, tasks taskstate.StateStore) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	workspace, err := repo.New(root)
	if err != nil {
		t.Fatalf("repo.New() error = %v", err)
	}
	server := newServerForComponents(workspace, tasks, "")
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

func assertMainOnlyToolError(t *testing.T, name string, result *mcp.CallToolResult) {
	t.Helper()
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("%s rejected result = %#v, want one tool error", name, result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "repository must be on main" {
		t.Errorf("%s error content = %#v, want main-only rejection", name, result.Content)
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
