package main

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestReplayMetadataTransportDerivesIdentityFromWireRequest(t *testing.T) {
	params := mcp.CallToolParams{Name: "workspace_create", Arguments: map[string]any{"task_id": "wire-task"}}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := jsonrpc.MakeID("same-wire-id")
	if err != nil {
		t.Fatal(err)
	}
	first := &jsonrpc.Request{ID: requestID, Method: "tools/call", Params: encodedParams}
	second := &jsonrpc.Request{ID: requestID, Method: "tools/call", Params: encodedParams}
	transport := &replayTestTransport{connection: &replayTestConnection{messages: []jsonrpc.Message{first, second}}}
	wrapped := &replayMetadataTransport{delegate: transport}
	connection, err := wrapped.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	firstMessage, err := connection.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondMessage, err := connection.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstParams := replayTestCallParams(t, firstMessage)
	secondParams := replayTestCallParams(t, secondMessage)
	firstID, firstSequence, ok := mcpReplayMetadata(firstParams.Meta)
	if !ok || firstID == "" || firstSequence != 1 {
		t.Fatalf("first derived metadata = %#v, want bounded identity and sequence 1", firstParams.Meta)
	}
	secondID, secondSequence, ok := mcpReplayMetadata(secondParams.Meta)
	if !ok || secondID != firstID || secondSequence != 2 {
		t.Fatalf("replayed derived metadata = %#v, want same identity and sequence 2", secondParams.Meta)
	}
}

func TestReplayMetadataTransportKeepsReadOnlyCallsUntouched(t *testing.T) {
	params := mcp.CallToolParams{Name: "repo_status"}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := jsonrpc.MakeID(float64(1))
	if err != nil {
		t.Fatal(err)
	}
	message := &jsonrpc.Request{ID: requestID, Method: "tools/call", Params: encodedParams}
	transport := &replayMetadataTransport{delegate: &replayTestTransport{connection: &replayTestConnection{messages: []jsonrpc.Message{message}}}}
	connection, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := connection.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := got.(*jsonrpc.Request)
	var paramsAfter mcp.CallToolParamsRaw
	if err := json.Unmarshal(request.Params, &paramsAfter); err != nil {
		t.Fatal(err)
	}
	if _, exists := paramsAfter.Meta[mcpSecurityRequestIDKey]; exists {
		t.Fatalf("read-only call received replay metadata: %#v", paramsAfter.Meta)
	}
}

func TestMCPMutatingCallWithoutPrivateMetadataWorksThroughProductionTransport(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "RepoWorker Test"},
		{"config", "user.email", "repoworker@example.invalid"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	server, plane, err := newServerWithProvider(root, t.TempDir(), testReplayPrincipalProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), (&replayMetadataTransport{delegate: serverTransport}), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "production-transport-test", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "workspace_create",
		Arguments: map[string]any{"task_id": "ordinary-client-call"},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("workspace_create without private metadata = %#v, error = %v", result, err)
	}
}

func TestMCPWireReplayIsRejectedAfterTransportDerivation(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "RepoWorker Test"},
		{"config", "user.email", "repoworker@example.invalid"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	server, plane, err := newServerWithProvider(root, t.TempDir(), testReplayPrincipalProvider(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	wire := &channelReplayConnection{incoming: make(chan jsonrpc.Message, 8), outgoing: make(chan jsonrpc.Message, 8), closed: make(chan struct{})}
	serverSession, err := server.Connect(context.Background(), (&replayMetadataTransport{delegate: &replayTestTransport{connection: wire}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	initParams, err := json.Marshal(mcp.InitializeParams{
		Capabilities:    &mcp.ClientCapabilities{},
		ClientInfo:      &mcp.Implementation{Name: "wire-replay-test", Version: "0.0.0"},
		ProtocolVersion: "2025-06-18",
	})
	if err != nil {
		t.Fatal(err)
	}
	initID, _ := jsonrpc.MakeID(float64(1))
	wire.incoming <- &jsonrpc.Request{ID: initID, Method: "initialize", Params: initParams}
	if response := replayTestResponse(t, wire.outgoing); response.Error != nil {
		t.Fatalf("initialize response error = %v", response.Error)
	}
	wire.incoming <- &jsonrpc.Request{Method: "notifications/initialized", Params: json.RawMessage("{}")}

	callParams, err := json.Marshal(mcp.CallToolParams{Name: "workspace_create", Arguments: map[string]any{"task_id": "wire-replay-task"}})
	if err != nil {
		t.Fatal(err)
	}
	callID, _ := jsonrpc.MakeID("fixed-call-id")
	call := &jsonrpc.Request{ID: callID, Method: "tools/call", Params: callParams}
	wire.incoming <- call
	if response := replayTestResponse(t, wire.outgoing); response.Error != nil {
		t.Fatalf("first wire call response error = %v", response.Error)
	}
	wire.incoming <- &jsonrpc.Request{ID: callID, Method: "tools/call", Params: callParams}
	if response := replayTestResponse(t, wire.outgoing); response.Error == nil {
		t.Fatal("replayed wire call succeeded; want request-level rejection")
	}
}

func replayTestResponse(t *testing.T, responses <-chan jsonrpc.Message) *jsonrpc.Response {
	t.Helper()
	select {
	case message := <-responses:
		response, ok := message.(*jsonrpc.Response)
		if !ok {
			t.Fatalf("response type = %T, want JSON-RPC response", message)
		}
		return response
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for JSON-RPC response")
		return nil
	}
}

func testReplayPrincipalProvider(t *testing.T) security.PrincipalProvider {
	t.Helper()
	provider, err := security.NewTrustedPrincipalProvider("mcp-transport-test")
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func replayTestCallParams(t *testing.T, message jsonrpc.Message) mcp.CallToolParamsRaw {
	t.Helper()
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		t.Fatalf("message type = %T, want request", message)
	}
	var params mcp.CallToolParamsRaw
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	return params
}

type replayTestTransport struct {
	connection mcp.Connection
}

func (t *replayTestTransport) Connect(context.Context) (mcp.Connection, error) {
	return t.connection, nil
}

type replayTestConnection struct {
	mu       sync.Mutex
	messages []jsonrpc.Message
}

func (c *replayTestConnection) Read(context.Context) (jsonrpc.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) == 0 {
		return nil, io.EOF
	}
	message := c.messages[0]
	c.messages = c.messages[1:]
	return message, nil
}

func (*replayTestConnection) Write(context.Context, jsonrpc.Message) error { return nil }
func (*replayTestConnection) Close() error                                 { return nil }
func (*replayTestConnection) SessionID() string                            { return "replay-test-session" }

type channelReplayConnection struct {
	incoming chan jsonrpc.Message
	outgoing chan jsonrpc.Message
	closed   chan struct{}
	once     sync.Once
}

func (c *channelReplayConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case message := <-c.incoming:
		return message, nil
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *channelReplayConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	select {
	case c.outgoing <- message:
		return nil
	case <-c.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *channelReplayConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (*channelReplayConnection) SessionID() string { return "wire-replay-session" }
