package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// replayMetadataTransport binds the replay guard to the actual JSON-RPC
// request received by the MCP transport. MCP clients are not required to know
// RepoWorker's private metadata keys, so the server derives those keys here
// from the protocol request ID and canonical call payload. This keeps replay
// protection usable through ordinary stdio tunnels without trusting tool
// arguments or inventing HTTP headers.
type replayMetadataTransport struct {
	delegate mcp.Transport
}

func (t *replayMetadataTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if t == nil || t.delegate == nil {
		return nil, errRequestRejected
	}
	conn, err := t.delegate.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &replayMetadataConnection{delegate: conn}, nil
}

type replayMetadataConnection struct {
	delegate mcp.Connection
	sequence atomic.Uint64
}

func (c *replayMetadataConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	message, err := c.delegate.Read(ctx)
	if err != nil {
		return nil, err
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok || request.Method != "tools/call" || !request.ID.IsValid() {
		return message, nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return message, nil
	}
	var name string
	if err := json.Unmarshal(params["name"], &name); err != nil {
		return message, nil
	}
	if _, mutating := mutatingMCPTools[name]; !mutating {
		return message, nil
	}

	requestID := replayRequestIdentity(request.ID, name, params["arguments"])
	sequence := c.sequence.Add(1)
	meta := make(mcp.Meta)
	if rawMeta, exists := params["_meta"]; exists {
		// Preserve unrelated protocol metadata, but never trust or retain a
		// caller-supplied value for RepoWorker's security keys.
		_ = json.Unmarshal(rawMeta, &meta)
	}
	if meta == nil {
		meta = make(mcp.Meta)
	}
	meta[mcpSecurityRequestIDKey] = requestID
	meta[mcpSecurityRequestSequenceKey] = sequence
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, errRequestRejected
	}
	params["_meta"] = encodedMeta
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, errRequestRejected
	}
	request.Params = encodedParams
	return request, nil
}

func (c *replayMetadataConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	return c.delegate.Write(ctx, message)
}

func (c *replayMetadataConnection) Close() error { return c.delegate.Close() }

func (c *replayMetadataConnection) SessionID() string { return c.delegate.SessionID() }

func replayRequestIdentity(id jsonrpc.ID, name string, rawArguments json.RawMessage) string {
	canonicalArguments := rawArguments
	var arguments any
	if len(rawArguments) != 0 && json.Unmarshal(rawArguments, &arguments) == nil {
		if encoded, err := json.Marshal(arguments); err == nil {
			canonicalArguments = encoded
		}
	}
	identity := struct {
		ID        any             `json:"id"`
		Method    string          `json:"method"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{ID: id.Raw(), Method: "tools/call", Name: name, Arguments: canonicalArguments}
	payload, _ := json.Marshal(identity)
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("rpc-%s", hex.EncodeToString(digest[:]))
}
