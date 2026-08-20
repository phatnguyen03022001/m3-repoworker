package security

import (
	"errors"
	"testing"
	"time"
)

func TestRequestReplayCacheBindsPrincipalTransportAndSession(t *testing.T) {
	cache, err := NewRequestReplayCache(2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Accept("transport-a", "session-a", "principal-a", "mcp-a", "request-a", 1); err != nil {
		t.Fatal(err)
	}
	for _, replay := range []struct {
		name        string
		transport   string
		authSession string
		principal   string
		mcpSession  string
		requestID   string
		sequence    uint64
	}{
		{name: "same request identity", transport: "transport-a", authSession: "session-a", principal: "principal-a", mcpSession: "mcp-a", requestID: "request-a", sequence: 2},
		{name: "same sequence", transport: "transport-a", authSession: "session-a", principal: "principal-a", mcpSession: "mcp-a", requestID: "request-b", sequence: 1},
		{name: "principal mismatch", transport: "transport-a", authSession: "session-a", principal: "principal-b", mcpSession: "mcp-a", requestID: "request-a", sequence: 1},
		{name: "transport mismatch", transport: "transport-b", authSession: "session-a", principal: "principal-a", mcpSession: "mcp-a", requestID: "request-a", sequence: 1},
		{name: "control session mismatch", transport: "transport-a", authSession: "session-b", principal: "principal-a", mcpSession: "mcp-a", requestID: "request-a", sequence: 1},
		{name: "MCP session mismatch", transport: "transport-a", authSession: "session-a", principal: "principal-a", mcpSession: "mcp-b", requestID: "request-a", sequence: 1},
	} {
		t.Run(replay.name, func(t *testing.T) {
			if err := cache.Accept(replay.transport, replay.authSession, replay.principal, replay.mcpSession, replay.requestID, replay.sequence); !errors.Is(err, ErrReplay) {
				t.Fatalf("Accept() error = %v, want ErrReplay", err)
			}
		})
	}
	if err := cache.Accept("transport-a", "session-a", "principal-a", "mcp-a", "request-c", 2); err != nil {
		t.Fatalf("new bounded entry error = %v", err)
	}
	if len(cache.entries) != 2 {
		t.Fatalf("cache entries = %d, want bounded size 2", len(cache.entries))
	}

	// A restart starts a fresh authenticated control-plane session/cache. The
	// old request identity is not carried into the new session's namespace.
	restarted, err := NewRequestReplayCache(2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Accept("transport-new", "session-new", "principal-a", "mcp-new", "request-a", 1); err != nil {
		t.Fatalf("fresh session request error = %v", err)
	}
}

func TestRequestReplayCacheRejectsInvalidMetadata(t *testing.T) {
	cache := NewDefaultRequestReplayCache()
	for _, test := range []struct {
		name     string
		request  string
		sequence uint64
	}{
		{name: "empty request", request: "", sequence: 1},
		{name: "zero sequence", request: "request-a", sequence: 0},
		{name: "unsafe request", request: "request/a", sequence: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := cache.Accept("transport-a", "session-a", "principal-a", "mcp-a", test.request, test.sequence); !errors.Is(err, ErrRequestMetadata) {
				t.Fatalf("Accept() error = %v, want ErrRequestMetadata", err)
			}
		})
	}
}
