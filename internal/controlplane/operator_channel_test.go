package controlplane

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tienphat/m3-repoworker/internal/operator"
	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestProductionOperatorChannelBindsPendingIntegration(t *testing.T) {
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("Apple container CLI is unavailable")
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "go.mod"), "module example.invalid/operator\n\ngo 1.26.6\n")
	writeFixtureFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	initFixtureGit(t, root)
	provider, err := security.NewTrustedPrincipalProvider("autonomous-caller")
	if err != nil {
		t.Fatal(err)
	}
	operatorAuthority, err := security.NewAuthenticatedOperatorAuthority("local-operator")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	plane, err := Open(context.Background(), Config{RepositoryRoot: root, StateRoot: stateRoot, PrincipalProvider: provider, OperatorAuthority: operatorAuthority})
	if err != nil {
		t.Fatal(err)
	}
	defer plane.Close()

	binding := security.ConfirmationBinding{
		Action:            "integration-action",
		RepositoryID:      plane.RepositoryID,
		PrincipalID:       plane.PrincipalID,
		SessionID:         plane.SessionID,
		GenerationID:      "generation-operator",
		FencingGeneration: 7,
		CandidateSnapshot: strings.Repeat("a", 64),
		PlanDigest:        strings.Repeat("b", 64),
	}
	remember := func() {
		plane.rememberConfirmationBinding(security.ConfirmationDestructive, binding)
	}
	remember()
	socketRoot, err := os.MkdirTemp("/private/tmp", "rwop-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	server, err := operator.NewServer(filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, plane.IssueOperatorConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		edit func(*security.ConfirmationBinding)
	}{
		{name: "action", edit: func(value *security.ConfirmationBinding) { value.Action = "different-action" }},
		{name: "repository", edit: func(value *security.ConfirmationBinding) { value.RepositoryID = strings.Repeat("c", 64) }},
		{name: "principal", edit: func(value *security.ConfirmationBinding) { value.PrincipalID = "different-caller" }},
		{name: "session", edit: func(value *security.ConfirmationBinding) { value.SessionID = "session-different" }},
		{name: "generation", edit: func(value *security.ConfirmationBinding) { value.GenerationID = "generation-different" }},
		{name: "fencing generation", edit: func(value *security.ConfirmationBinding) { value.FencingGeneration++ }},
		{name: "candidate snapshot", edit: func(value *security.ConfirmationBinding) { value.CandidateSnapshot = strings.Repeat("d", 64) }},
		{name: "plan digest", edit: func(value *security.ConfirmationBinding) { value.PlanDigest = strings.Repeat("e", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			remember()
			forged := binding
			test.edit(&forged)
			if _, err := operator.Approve(context.Background(), filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, security.OperatorConfirmationRequest{Binding: forged, Class: security.ConfirmationDestructive, TTL: time.Minute}); err == nil {
				t.Fatal("binding mismatch unexpectedly approved")
			}
		})
	}

	// The real production channel can approve the pending integration, and
	// returns only the opaque one-time token to its operator caller.
	remember()
	approved, err := operator.Approve(context.Background(), filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute})
	if err != nil || approved.Token == "" {
		t.Fatalf("production operator approval = %#v, %v", approved, err)
	}

	// Consumption remains atomic at the security boundary.
	remember()
	concurrent, err := operator.Approve(context.Background(), filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := security.Request{SessionID: plane.SessionID, PrincipalID: plane.PrincipalID, RepositoryID: plane.RepositoryID, Nonce: plane.session.Nonce, Capability: security.CapabilityIntegrate, Target: security.TargetLiveRepository, TrustedIntegrationRef: plane.trustedRef, ConfirmationToken: concurrent.Token, ConfirmationBinding: binding}
	securityBinding := security.Binding{RepositoryID: plane.RepositoryID, FilesystemID: plane.FilesystemID, LiveRepository: plane.Repository.LiveRoot(), TaskWorkspace: "/private/tmp/operator-workspace", WorkspaceID: binding.GenerationID}
	type authorizationResult struct {
		err       error
		nextNonce string
	}
	results := make(chan authorizationResult, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			decision, callErr := plane.Security.Authorize(context.Background(), request, securityBinding)
			results <- authorizationResult{err: callErr, nextNonce: decision.NextNonce}
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	var nextNonce string
	for result := range results {
		if result.err == nil {
			successes++
			nextNonce = result.nextNonce
		} else if !errors.Is(result.err, security.ErrDenied) && !errors.Is(result.err, security.ErrReplay) {
			t.Fatalf("concurrent consume error = %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent channel confirmation successes = %d, want exactly 1", successes)
	}
	plane.session.Nonce = nextNonce

	// A short-lived channel-issued token is rejected after expiry by the
	// security engine, even though the socket itself remains available.
	remember()
	expired, err := operator.Approve(context.Background(), filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := plane.Security.Authorize(context.Background(), security.Request{SessionID: plane.SessionID, PrincipalID: plane.PrincipalID, RepositoryID: plane.RepositoryID, Nonce: plane.session.Nonce, Capability: security.CapabilityIntegrate, Target: security.TargetLiveRepository, TrustedIntegrationRef: plane.trustedRef, ConfirmationToken: expired.Token, ConfirmationBinding: binding}, security.Binding{RepositoryID: plane.RepositoryID, FilesystemID: plane.FilesystemID, LiveRepository: plane.Repository.LiveRoot(), TaskWorkspace: "/private/tmp/operator-workspace", WorkspaceID: binding.GenerationID}); err == nil {
		t.Fatal("expired channel confirmation unexpectedly authorized")
	}

	// Approval is bound to the current session. Reopening creates a new
	// authenticated session and does not resurrect the old confirmation.
	remember()
	oldSessionConfirmation, err := operator.Approve(context.Background(), filepath.Join(socketRoot, "operator.sock"), key, operatorAuthority.OperatorID, security.OperatorConfirmationRequest{Binding: binding, Class: security.ConfirmationDestructive, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plane.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), Config{RepositoryRoot: root, StateRoot: stateRoot, PrincipalProvider: provider, OperatorAuthority: operatorAuthority})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.SessionID == binding.SessionID {
		t.Fatal("reopen reused the old authenticated session")
	}
	if _, err := reopened.Security.Authorize(context.Background(), security.Request{SessionID: binding.SessionID, PrincipalID: binding.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: plane.session.Nonce, Capability: security.CapabilityIntegrate, Target: security.TargetLiveRepository, TrustedIntegrationRef: plane.trustedRef, ConfirmationToken: oldSessionConfirmation.Token, ConfirmationBinding: binding}, security.Binding{RepositoryID: binding.RepositoryID, FilesystemID: reopened.FilesystemID, LiveRepository: reopened.Repository.LiveRoot(), TaskWorkspace: "/private/tmp/operator-workspace", WorkspaceID: binding.GenerationID}); err == nil {
		t.Fatal("old session confirmation unexpectedly authorized after reopen")
	}
}
