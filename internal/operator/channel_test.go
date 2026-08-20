package operator

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestPrivateSocketOperatorApprovalAndReplay(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "rwop-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	binding := security.ConfirmationBinding{Action: "integrate:plan", RepositoryID: strings.Repeat("a", 64), PrincipalID: "caller-a", GenerationID: "generation-a", FencingGeneration: 7, CandidateSnapshot: strings.Repeat("b", 64), PlanDigest: strings.Repeat("c", 64)}
	operatorAuthority, err := security.NewAuthenticatedOperatorAuthority("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := security.NewEngine(security.Policy{Version: security.PolicyVersion, Capabilities: []security.Capability{security.CapabilityRepoRead}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.EnrollRepository(context.Background(), binding.PrincipalID, binding.RepositoryID, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	session, err := engine.OpenAuthenticatedSession(context.Background(), security.Principal{ID: binding.PrincipalID, TransportID: "transport-a", AuthenticationID: strings.Repeat("e", 64), ExpiresAt: time.Now().Add(time.Hour)}, binding.RepositoryID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding.SessionID = session.ID
	approve := func(ctx context.Context, request security.OperatorConfirmationRequest) (security.Confirmation, error) {
		return engine.IssueOperatorConfirmation(ctx, operatorAuthority, request)
	}
	socketPath := filepath.Join(root, "operator.sock")
	server, err := NewServer(socketPath, key, "operator-a", approve)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v (socket=%s)", err, socketPath)
	}
	defer server.Close()
	info, err := os.Stat(socketPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	confirmation, err := Approve(context.Background(), socketPath, key, "operator-a", security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute})
	if err != nil || confirmation.Token == "" {
		t.Fatalf("operator approval = %#v, %v; want a token from the private channel", confirmation, err)
	}
	if _, err := engine.Authorize(context.Background(), security.Request{SessionID: session.ID, PrincipalID: binding.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: security.CapabilityRepoRead, Target: security.TargetLiveRepository, Path: "README.md"}, security.Binding{RepositoryID: binding.RepositoryID, FilesystemID: strings.Repeat("d", 64), LiveRepository: "/repo", TaskWorkspace: "/workspace/task", WorkspaceID: binding.GenerationID}); err != nil {
		t.Fatalf("post-approval security control = %v", err)
	}
	if _, err := Approve(context.Background(), socketPath, key, "caller-a", security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute}); err == nil {
		t.Fatal("self operator approval unexpectedly succeeded")
	}
	selfAuthority, err := security.NewAuthenticatedOperatorAuthority("caller-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.IssueOperatorConfirmation(security.WithOperatorAuthentication(context.Background(), "caller-a"), selfAuthority, security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute}); !errors.Is(err, security.ErrDenied) {
		t.Fatalf("self authority error = %v, want ErrDenied", err)
	}

	// Reusing the exact signed wire request (including its operator nonce) is
	// rejected by the private channel before a second confirmation is minted.
	replayRequest := wireRequest{OperatorID: "operator-a", Nonce: "operator-fixed-replay", Class: security.ConfirmationDestructive, TTLSeconds: 60, Binding: binding}
	payload, err := json.Marshal(signaturePayloadFor(replayRequest))
	if err != nil {
		t.Fatal(err)
	}
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write(payload)
	replayRequest.Signature = hex.EncodeToString(hash.Sum(nil))
	frame, err := json.Marshal(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	call := func() wireResponse {
		conn, dialErr := net.Dial("unix", socketPath)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer conn.Close()
		if _, writeErr := conn.Write(append(frame, '\n')); writeErr != nil {
			t.Fatal(writeErr)
		}
		responseFrame, readErr := bufio.NewReader(conn).ReadBytes('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response wireResponse
		if unmarshalErr := json.Unmarshal(responseFrame, &response); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return response
	}
	if response := call(); response.Error != "" || response.Confirmation == nil {
		t.Fatalf("first signed wire request = %#v, want approval", response)
	}
	if response := call(); response.Error == "" {
		t.Fatalf("replayed signed wire request = %#v, want rejection", response)
	}
}
