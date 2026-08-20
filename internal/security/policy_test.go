package security

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPolicy() Policy {
	return Policy{
		Version: PolicyVersion,
		Capabilities: []Capability{
			CapabilityRepoRead, CapabilityWorkspaceRead, CapabilityWorkspaceWrite,
			CapabilityExecute, CapabilityIntegrate, CapabilityPublish,
			CapabilityNetworkRegistry,
		},
		Mounts:    []MountRule{{Source: MountTaskWorkspace, Target: "/workspace", ReadOnly: true}},
		Network:   NetworkPolicy{Mode: NetworkRegistry, RegistryDomain: []string{"registry.example", "registry.example"}},
		Execution: ExecutionPolicy{AllowedExecutables: []string{"/usr/bin/go"}, MaxArguments: 4, MaxArgBytes: 1024},
	}
}

func testBinding() Binding {
	return Binding{
		RepositoryID:   strings.Repeat("a", 64),
		FilesystemID:   strings.Repeat("b", 64),
		LiveRepository: "/repo",
		TaskWorkspace:  "/state/workspace/gen_1",
		WorkspaceID:    "gen_1",
	}
}

func testPrincipal(id string) Principal {
	return Principal{ID: id, TransportID: "transport-" + id, AuthenticationID: strings.Repeat("c", 64), ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func TestDenyByDefaultAndNonceReplayProtection(t *testing.T) {
	engine, err := NewEngine(Policy{Version: PolicyVersion})
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	_, _, err = engine.EnrollRepository(context.Background(), "principal-a", testBinding().RepositoryID, testBinding().FilesystemID)
	if err != nil {
		t.Fatalf("EnrollRepository() error = %v", err)
	}
	session, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("principal-a"), testBinding().RepositoryID, time.Minute)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: session.RepositoryID, Nonce: session.Nonce, Capability: CapabilityRepoRead, Target: TargetLiveRepository, Path: "README.md"}
	if _, err := engine.Authorize(context.Background(), request, testBinding()); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied capability error = %v, want ErrDenied", err)
	}
	if _, err := engine.Authorize(context.Background(), request, testBinding()); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed denied request error = %v, want ErrReplay", err)
	}
	if events := engine.AuditEvents(); len(events) != 3 || events[1].Outcome != "denied" {
		t.Fatalf("audit events = %#v", events)
	}
}

func TestAllowedRequestRotatesNonceAndCompilesIsolatedRuntimePolicy(t *testing.T) {
	engine, err := NewEngine(testPolicy())
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	binding := testBinding()
	_, trustedRef, err := engine.EnrollRepository(context.Background(), "principal-a", binding.RepositoryID, binding.FilesystemID)
	if err != nil {
		t.Fatalf("EnrollRepository() error = %v", err)
	}
	session, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("principal-a"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: CapabilityWorkspaceWrite, Target: TargetTaskWorkspace, Path: "src/main.go"}
	decision, err := engine.Authorize(context.Background(), request, binding)
	if err != nil || !decision.Allowed || decision.NextNonce == session.Nonce {
		t.Fatalf("Authorize() = %#v, %v", decision, err)
	}
	if len(decision.Policy.Mounts) != 1 || !strings.HasSuffix(decision.Policy.Mounts[0].Source, "gen_1") || decision.Policy.Mounts[0].ReadOnly {
		t.Fatalf("compiled policy = %#v", decision.Policy)
	}
	if decision.Policy.Network.Mode != NetworkRegistry || len(decision.Policy.Network.RegistryDomain) != 1 {
		t.Fatalf("compiled network policy = %#v", decision.Policy.Network)
	}
	_ = trustedRef
}

func TestDestructiveAndPublicationNeedTrustedOneTimeConfirmations(t *testing.T) {
	engine, err := NewEngine(testPolicy())
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	binding := testBinding()
	_, trustedRef, err := engine.EnrollRepository(context.Background(), "principal-a", binding.RepositoryID, binding.FilesystemID)
	if err != nil {
		t.Fatalf("EnrollRepository() error = %v", err)
	}
	session, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("principal-a"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	operator, err := NewExplicitOperatorAuthority("operator-a")
	if err != nil {
		t.Fatalf("NewExplicitOperatorAuthority() error = %v", err)
	}
	confirmationBinding := ConfirmationBinding{Action: "integrate-readme", RepositoryID: binding.RepositoryID, PrincipalID: session.PrincipalID, SessionID: session.ID, GenerationID: binding.WorkspaceID, FencingGeneration: 1, CandidateSnapshot: strings.Repeat("d", 64), PlanDigest: strings.Repeat("e", 64)}
	confirmation, err := engine.IssueOperatorConfirmation(context.Background(), operator, OperatorConfirmationRequest{Binding: confirmationBinding, Class: ConfirmationDestructive, TTL: time.Minute})
	if err != nil {
		t.Fatalf("IssueConfirmation() error = %v", err)
	}
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: CapabilityIntegrate, Target: TargetLiveRepository, Path: "README.md", TrustedIntegrationRef: trustedRef, ConfirmationToken: confirmation.Token, ConfirmationBinding: confirmationBinding}
	decision, err := engine.Authorize(context.Background(), request, binding)
	if err != nil || !decision.Allowed {
		t.Fatalf("integrate Authorize() = %#v, %v", decision, err)
	}
	request.Nonce = decision.NextNonce
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("reused confirmation error = %v, want ErrDenied", err)
	}

	publicationBinding := confirmationBinding
	publicationBinding.Action = "publish-readme"
	publicationBinding.PlanDigest = strings.Repeat("f", 64)
	publication, err := engine.IssueOperatorConfirmation(context.Background(), operator, OperatorConfirmationRequest{Binding: publicationBinding, Class: ConfirmationPublication, TTL: time.Minute})
	if err != nil {
		t.Fatalf("publication confirmation error = %v", err)
	}
	request = Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: request.Nonce, Capability: CapabilityPublish, Target: TargetLiveRepository, TrustedIntegrationRef: trustedRef, ConfirmationToken: publication.Token, ConfirmationBinding: publicationBinding}
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrReplay) {
		t.Fatalf("publication with stale nonce error = %v, want ErrReplay", err)
	}
}

func TestOperatorConfirmationRejectsSelfApprovalScopeChangeExpiryAndReplay(t *testing.T) {
	engine, err := NewEngine(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	_, trustedRef, err := engine.EnrollRepository(context.Background(), "caller-a", binding.RepositoryID, binding.FilesystemID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("caller-a"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewExplicitOperatorAuthority("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	base := ConfirmationBinding{Action: "integrate:plan-a", RepositoryID: binding.RepositoryID, PrincipalID: session.PrincipalID, SessionID: session.ID, GenerationID: binding.WorkspaceID, FencingGeneration: 1, CandidateSnapshot: strings.Repeat("d", 64), PlanDigest: strings.Repeat("e", 64)}
	self, _ := NewExplicitOperatorAuthority("caller-a")
	if _, err := engine.IssueOperatorConfirmation(context.Background(), self, OperatorConfirmationRequest{Binding: base, Class: ConfirmationDestructive, TTL: time.Minute}); !errors.Is(err, ErrDenied) {
		t.Fatalf("self approval error = %v, want ErrDenied", err)
	}
	confirmation, err := engine.IssueOperatorConfirmation(context.Background(), authority, OperatorConfirmationRequest{Binding: base, Class: ConfirmationDestructive, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: CapabilityIntegrate, Target: TargetLiveRepository, TrustedIntegrationRef: trustedRef, ConfirmationToken: confirmation.Token, ConfirmationBinding: base}
	scopeChanged := base
	scopeChanged.CandidateSnapshot = strings.Repeat("f", 64)
	request.ConfirmationBinding = scopeChanged
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("scope mismatch error = %v, want ErrDenied", err)
	}
	expiring := base
	expiring.Action = "integrate:expiring"
	expiringSession, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("caller-a"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expiring.SessionID = expiringSession.ID
	expiredConfirmation, err := engine.IssueOperatorConfirmation(context.Background(), authority, OperatorConfirmationRequest{Binding: expiring, Class: ConfirmationDestructive, TTL: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	request.SessionID = expiringSession.ID
	request.Nonce = expiringSession.Nonce
	request.ConfirmationToken = expiredConfirmation.Token
	request.ConfirmationBinding = expiring
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired confirmation error = %v, want ErrDenied", err)
	}
}

func TestSessionIsolationRevocationAndConcurrentConfirmationConsume(t *testing.T) {
	engine, err := NewEngine(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	_, trustedRef, err := engine.EnrollRepository(context.Background(), "caller-a", binding.RepositoryID, binding.FilesystemID)
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("caller-a"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := engine.OpenAuthenticatedSession(context.Background(), testPrincipal("caller-b"), binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	forged := Request{SessionID: sessionA.ID, PrincipalID: sessionB.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: sessionA.Nonce, Capability: CapabilityRepoRead, Target: TargetLiveRepository, Path: "README.md"}
	if _, err := engine.Authorize(context.Background(), forged, binding); !errors.Is(err, ErrReplay) {
		t.Fatalf("forged principal request error = %v, want ErrReplay", err)
	}
	otherRepository := binding
	otherRepository.RepositoryID = strings.Repeat("f", 64)
	if _, _, err := engine.EnrollRepository(context.Background(), "caller-a", otherRepository.RepositoryID, otherRepository.FilesystemID); err != nil {
		t.Fatal(err)
	}
	wrongRepository := Request{SessionID: sessionA.ID, PrincipalID: sessionA.PrincipalID, RepositoryID: otherRepository.RepositoryID, Nonce: sessionA.Nonce, Capability: CapabilityRepoRead, Target: TargetLiveRepository, Path: "README.md"}
	if _, err := engine.Authorize(context.Background(), wrongRepository, otherRepository); !errors.Is(err, ErrReplay) {
		t.Fatalf("repository mismatch error = %v, want ErrReplay", err)
	}
	if err := engine.RevokeSession(context.Background(), sessionB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Authorize(context.Background(), Request{SessionID: sessionB.ID, PrincipalID: sessionB.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: sessionB.Nonce, Capability: CapabilityRepoRead, Target: TargetLiveRepository, Path: "README.md"}, binding); !errors.Is(err, ErrExpired) {
		t.Fatalf("revoked session error = %v, want ErrExpired", err)
	}

	operator, err := NewExplicitOperatorAuthority("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	confirmationBinding := ConfirmationBinding{Action: "integrate:concurrent", RepositoryID: binding.RepositoryID, PrincipalID: sessionA.PrincipalID, SessionID: sessionA.ID, GenerationID: binding.WorkspaceID, FencingGeneration: 1, CandidateSnapshot: strings.Repeat("d", 64), PlanDigest: strings.Repeat("e", 64)}
	confirmation, err := engine.IssueOperatorConfirmation(context.Background(), operator, OperatorConfirmationRequest{Binding: confirmationBinding, Class: ConfirmationDestructive, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{SessionID: sessionA.ID, PrincipalID: sessionA.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: sessionA.Nonce, Capability: CapabilityIntegrate, Target: TargetLiveRepository, Path: "README.md", TrustedIntegrationRef: trustedRef, ConfirmationToken: confirmation.Token, ConfirmationBinding: confirmationBinding}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, callErr := engine.Authorize(context.Background(), request, binding)
			results <- callErr
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	rejections := 0
	for callErr := range results {
		if callErr == nil {
			successes++
		} else if errors.Is(callErr, ErrReplay) || errors.Is(callErr, ErrDenied) {
			rejections++
		} else {
			t.Fatalf("concurrent consume error = %v", callErr)
		}
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("concurrent confirmation consume: successes=%d rejections=%d, want 1/1", successes, rejections)
	}
}

func TestAuthenticatedSessionExpiresClosed(t *testing.T) {
	engine, err := NewEngine(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	if _, _, err := engine.EnrollRepository(context.Background(), "caller-a", binding.RepositoryID, binding.FilesystemID); err != nil {
		t.Fatal(err)
	}
	principal := testPrincipal("caller-a")
	principal.ExpiresAt = time.Now().UTC().Add(20 * time.Millisecond)
	session, err := engine.OpenAuthenticatedSession(context.Background(), principal, binding.RepositoryID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: CapabilityRepoRead, Target: TargetLiveRepository, Path: "README.md"}
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired session error = %v, want ErrExpired", err)
	}
}

func TestCompilationRejectsLiveWorkspaceOverlapFullNetworkAndHostShell(t *testing.T) {
	binding := testBinding()
	bad := binding
	bad.TaskWorkspace = "/repo/task"
	if _, err := Compile(testPolicy(), bad); !errors.Is(err, ErrDenied) {
		t.Fatalf("overlapping workspace Compile() error = %v", err)
	}
	full := testPolicy()
	full.Network = NetworkPolicy{Mode: NetworkFull}
	if _, err := Compile(full, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("full network Compile() error = %v", err)
	}
	for _, executable := range []string{"/bin/sh", "/bin/bash"} {
		if _, err := CompileExecution(ExecutionPolicy{AllowedExecutables: []string{executable}}, ExecutionSpec{Backend: "apple-container", Executable: executable, CWD: "/workspace"}); !errors.Is(err, ErrDenied) {
			t.Fatalf("shell executable %q accepted: %v", executable, err)
		}
	}
	if _, err := CompileExecution(testPolicy().Execution, ExecutionSpec{Backend: "host", Executable: "/usr/bin/go", CWD: "/workspace"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("host backend accepted: %v", err)
	}
}

func TestRedactionNeverReturnsCredentialMaterial(t *testing.T) {
	for _, secret := range []string{"Authorization: Bearer hidden", "-----BEGIN PRIVATE KEY-----", "ghp_secret"} {
		if got := redact(secret); got == secret || strings.Contains(got, "hidden") || strings.Contains(got, "secret") {
			t.Fatalf("redact(%q) = %q", secret, got)
		}
	}
	if got := redact(strings.Repeat("x", 2000)); len(got) > 1027 {
		t.Fatalf("bounded redaction length = %d", len(got))
	}
}
