package security

import (
	"context"
	"errors"
	"strings"
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

func TestDenyByDefaultAndNonceReplayProtection(t *testing.T) {
	engine, err := NewEngine(Policy{Version: PolicyVersion})
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	_, _, err = engine.EnrollRepository(context.Background(), "principal-a", testBinding().RepositoryID, testBinding().FilesystemID)
	if err != nil {
		t.Fatalf("EnrollRepository() error = %v", err)
	}
	session, err := engine.OpenSession(context.Background(), "principal-a", testBinding().RepositoryID, time.Minute)
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
	session, err := engine.OpenSession(context.Background(), "principal-a", binding.RepositoryID, time.Minute)
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
	session, err := engine.OpenSession(context.Background(), "principal-a", binding.RepositoryID, time.Minute)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	confirmation, err := engine.IssueConfirmation(context.Background(), session.ID, ConfirmationDestructive, time.Minute)
	if err != nil {
		t.Fatalf("IssueConfirmation() error = %v", err)
	}
	request := Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: session.Nonce, Capability: CapabilityIntegrate, Target: TargetLiveRepository, Path: "README.md", TrustedIntegrationRef: trustedRef, ConfirmationToken: confirmation.Token}
	decision, err := engine.Authorize(context.Background(), request, binding)
	if err != nil || !decision.Allowed {
		t.Fatalf("integrate Authorize() = %#v, %v", decision, err)
	}
	request.Nonce = decision.NextNonce
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("reused confirmation error = %v, want ErrDenied", err)
	}

	publication, err := engine.IssueConfirmation(context.Background(), session.ID, ConfirmationPublication, time.Minute)
	if err != nil {
		t.Fatalf("publication confirmation error = %v", err)
	}
	request = Request{SessionID: session.ID, PrincipalID: session.PrincipalID, RepositoryID: binding.RepositoryID, Nonce: request.Nonce, Capability: CapabilityPublish, Target: TargetLiveRepository, TrustedIntegrationRef: trustedRef, ConfirmationToken: publication.Token}
	if _, err := engine.Authorize(context.Background(), request, binding); !errors.Is(err, ErrReplay) {
		t.Fatalf("publication with stale nonce error = %v, want ErrReplay", err)
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
