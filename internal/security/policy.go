// Package security contains the typed M3 execution-security authority.
// Policies are Go domain values compiled into narrow runtime inputs; there is
// intentionally no generic policy language or host-shell capability.
package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const PolicyVersion = "m3.2-v1"

var (
	ErrDenied  = errors.New("security policy denied")
	ErrReplay  = errors.New("security request replayed")
	ErrExpired = errors.New("security session expired")
)

type Capability string

const (
	CapabilityRepoRead        Capability = "repo.read"
	CapabilityRepoSearch      Capability = "repo.search"
	CapabilityWorkspaceRead   Capability = "workspace.read"
	CapabilityWorkspaceWrite  Capability = "workspace.write"
	CapabilityExecute         Capability = "runtime.execute"
	CapabilityRuntimeCreate   Capability = "runtime.create"
	CapabilityNetworkRegistry Capability = "network.registry"
	CapabilityIntegrate       Capability = "repository.integrate"
	CapabilityPublish         Capability = "repository.publish"
	CapabilityCredentialRead  Capability = "credential.read"
)

type TargetKind string

const (
	TargetLiveRepository TargetKind = "live_repository"
	TargetTaskWorkspace  TargetKind = "task_workspace"
	TargetRuntime        TargetKind = "runtime"
)

type ConfirmationClass string

const (
	ConfirmationNone        ConfirmationClass = "none"
	ConfirmationDestructive ConfirmationClass = "destructive"
	ConfirmationPublication ConfirmationClass = "publication"
)

type NetworkMode string

const (
	NetworkNone     NetworkMode = "none"
	NetworkRegistry NetworkMode = "registry"
	NetworkFull     NetworkMode = "full"
)

type MountSource string

const (
	MountTaskWorkspace MountSource = "task_workspace"
)

type MountRule struct {
	Source   MountSource
	Target   string
	ReadOnly bool
}

type NetworkPolicy struct {
	Mode           NetworkMode
	RegistryDomain []string
}

type CredentialPolicy struct {
	AllowedScopes []string
	Provider      string
}

type ExecutionPolicy struct {
	AllowedExecutables []string
	MaxArgBytes        int
	MaxArguments       int
}

type Policy struct {
	Version      string
	Capabilities []Capability
	Mounts       []MountRule
	Network      NetworkPolicy
	Credentials  CredentialPolicy
	Execution    ExecutionPolicy
}

type RepositoryEnrollment struct {
	RepositoryID     string
	FilesystemID     string
	TrustedRefDigest string
	EnrolledAt       time.Time
}

type Session struct {
	ID           string
	PrincipalID  string
	RepositoryID string
	Nonce        string
	ExpiresAt    time.Time
}

type Confirmation struct {
	Token     string
	Class     ConfirmationClass
	SessionID string
	ExpiresAt time.Time
}

type Request struct {
	SessionID             string
	PrincipalID           string
	RepositoryID          string
	Nonce                 string
	Capability            Capability
	Target                TargetKind
	Path                  string
	TrustedIntegrationRef string
	ConfirmationToken     string
	Execution             ExecutionSpec
}

type Binding struct {
	RepositoryID   string
	FilesystemID   string
	LiveRepository string
	TaskWorkspace  string
	WorkspaceID    string
}

type ExecutionSpec struct {
	Backend    string
	Executable string
	Arguments  []string
	CWD        string
}

type CompiledMount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type CompiledCredentials struct {
	Provider string
	Scopes   []string
}

type CompiledPolicy struct {
	Version     string
	Mounts      []CompiledMount
	Network     NetworkPolicy
	Credentials CompiledCredentials
}

type CompiledExecution struct {
	Backend    string
	Executable string
	Arguments  []string
	CWD        string
}

type Decision struct {
	Allowed    bool
	NextNonce  string
	Policy     CompiledPolicy
	Execution  CompiledExecution
	AuditEvent AuditEvent
}

type AuditEvent struct {
	At           time.Time
	PrincipalID  string
	SessionID    string
	RepositoryID string
	Capability   Capability
	Outcome      string
	Detail       string
}

type Engine struct {
	policy        Policy
	mu            sync.Mutex
	enrollments   map[string]RepositoryEnrollment
	sessions      map[string]Session
	confirmations map[string]Confirmation
	audit         []AuditEvent
}

func NewEngine(policy Policy) (*Engine, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, ErrDenied
	}
	return &Engine{policy: policy, enrollments: map[string]RepositoryEnrollment{}, sessions: map[string]Session{}, confirmations: map[string]Confirmation{}}, nil
}

func (e *Engine) EnrollRepository(ctx context.Context, principalID string, repositoryID, filesystemID string) (RepositoryEnrollment, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx == nil || !validID(principalID) || !validIdentity(repositoryID) || !validIdentity(filesystemID) {
		return RepositoryEnrollment{}, "", ErrDenied
	}
	ref, err := newOpaque("ref_")
	if err != nil {
		return RepositoryEnrollment{}, "", ErrDenied
	}
	digest := digestString(ref)
	enrollment := RepositoryEnrollment{RepositoryID: repositoryID, FilesystemID: filesystemID, TrustedRefDigest: digest, EnrolledAt: time.Now().UTC()}
	e.enrollments[repositoryID] = enrollment
	e.appendAuditLocked(AuditEvent{At: time.Now().UTC(), PrincipalID: principalID, RepositoryID: repositoryID, Outcome: "enrolled", Detail: "repository enrolled"})
	return enrollment, ref, nil
}

func (e *Engine) OpenSession(ctx context.Context, principalID, repositoryID string, ttl time.Duration) (Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx == nil || !validID(principalID) || !validIdentity(repositoryID) || ttl <= 0 || ttl > 24*time.Hour {
		return Session{}, ErrDenied
	}
	if _, ok := e.enrollments[repositoryID]; !ok {
		return Session{}, ErrDenied
	}
	id, err := newOpaque("session_")
	if err != nil {
		return Session{}, ErrDenied
	}
	nonce, err := newOpaque("nonce_")
	if err != nil {
		return Session{}, ErrDenied
	}
	session := Session{ID: id, PrincipalID: principalID, RepositoryID: repositoryID, Nonce: nonce, ExpiresAt: time.Now().UTC().Add(ttl)}
	e.sessions[id] = session
	return session, nil
}

func (e *Engine) IssueConfirmation(ctx context.Context, sessionID string, class ConfirmationClass, ttl time.Duration) (Confirmation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx == nil || !validOpaque(sessionID) || (class != ConfirmationDestructive && class != ConfirmationPublication) || ttl <= 0 || ttl > time.Hour {
		return Confirmation{}, ErrDenied
	}
	session, ok := e.sessions[sessionID]
	if !ok || !session.ExpiresAt.After(time.Now().UTC()) {
		return Confirmation{}, ErrExpired
	}
	token, err := newOpaque("confirm_")
	if err != nil {
		return Confirmation{}, ErrDenied
	}
	confirmation := Confirmation{Token: token, Class: class, SessionID: sessionID, ExpiresAt: time.Now().UTC().Add(ttl)}
	e.confirmations[token] = confirmation
	return confirmation, nil
}

func (e *Engine) Authorize(ctx context.Context, request Request, binding Binding) (Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx == nil {
		return Decision{}, ErrDenied
	}
	event := AuditEvent{At: time.Now().UTC(), PrincipalID: request.PrincipalID, SessionID: request.SessionID, RepositoryID: request.RepositoryID, Capability: request.Capability}
	deny := func(err error, detail string) (Decision, error) {
		event.Outcome = "denied"
		event.Detail = redact(detail)
		e.appendAuditLocked(event)
		return Decision{}, err
	}
	session, ok := e.sessions[request.SessionID]
	if !ok || !session.ExpiresAt.After(time.Now().UTC()) {
		return deny(ErrExpired, "session expired")
	}
	if session.PrincipalID != request.PrincipalID || session.RepositoryID != request.RepositoryID || request.Nonce == "" || request.Nonce != session.Nonce {
		return deny(ErrReplay, "session binding or nonce rejected")
	}
	enrollment, enrolled := e.enrollments[request.RepositoryID]
	if !enrolled || binding.RepositoryID != request.RepositoryID || binding.FilesystemID == "" || enrollment.FilesystemID != binding.FilesystemID {
		return deny(ErrDenied, "repository enrollment rejected")
	}
	// Consume the nonce before evaluating capability details. A denied request
	// can never be replayed with a different capability or path.
	nextNonce, err := newOpaque("nonce_")
	if err != nil {
		return deny(ErrDenied, "nonce generation failed")
	}
	session.Nonce = nextNonce
	e.sessions[session.ID] = session
	if !containsCapability(e.policy.Capabilities, request.Capability) {
		return deny(ErrDenied, "capability denied")
	}
	compiled, err := Compile(e.policy, binding)
	if err != nil {
		return deny(ErrDenied, "runtime policy compilation failed")
	}
	if err := validateTarget(request, binding); err != nil {
		return deny(ErrDenied, "target denied")
	}
	if class := requiredConfirmation(request.Capability); class != ConfirmationNone {
		if err := e.consumeConfirmationLocked(request.SessionID, request.ConfirmationToken, class); err != nil {
			return deny(ErrDenied, "confirmation denied")
		}
	}
	if request.Capability == CapabilityIntegrate || request.Capability == CapabilityPublish {
		if err := e.validateTrustedRefLocked(request.RepositoryID, request.TrustedIntegrationRef); err != nil {
			return deny(ErrDenied, "trusted integration reference denied")
		}
	}
	decision := Decision{Allowed: true, NextNonce: nextNonce, Policy: compiled}
	if request.Capability == CapabilityExecute {
		decision.Execution, err = CompileExecution(e.policy.Execution, request.Execution)
		if err != nil {
			return deny(ErrDenied, "execution denied")
		}
	}
	event.Outcome = "allowed"
	event.Detail = "typed policy allowed"
	e.appendAuditLocked(event)
	decision.AuditEvent = event
	return decision, nil
}

func (e *Engine) AuditEvents() []AuditEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := append([]AuditEvent(nil), e.audit...)
	return result
}

func Compile(policy Policy, binding Binding) (CompiledPolicy, error) {
	if err := validatePolicy(policy); err != nil || !validIdentity(binding.RepositoryID) || binding.FilesystemID == "" || !validOpaque(binding.WorkspaceID) || !safeAbsolute(binding.TaskWorkspace) || !safeAbsolute(binding.LiveRepository) || sameOrWithin(binding.LiveRepository, binding.TaskWorkspace) || sameOrWithin(binding.TaskWorkspace, binding.LiveRepository) {
		return CompiledPolicy{}, ErrDenied
	}
	mounts := make([]CompiledMount, 0, 1)
	for _, mount := range policy.Mounts {
		if mount.Source != MountTaskWorkspace || !safeAbsolute(mount.Target) || sameOrWithin(binding.LiveRepository, binding.TaskWorkspace) {
			return CompiledPolicy{}, ErrDenied
		}
		readOnly := mount.ReadOnly
		if containsCapability(policy.Capabilities, CapabilityWorkspaceWrite) {
			readOnly = false
		}
		mounts = append(mounts, CompiledMount{Source: binding.TaskWorkspace, Target: mount.Target, ReadOnly: readOnly})
	}
	if len(mounts) == 0 {
		mounts = append(mounts, CompiledMount{Source: binding.TaskWorkspace, Target: "/workspace", ReadOnly: !containsCapability(policy.Capabilities, CapabilityWorkspaceWrite)})
	}
	network := policy.Network
	if network.Mode == "" {
		network.Mode = NetworkNone
	}
	if network.Mode == NetworkFull {
		return CompiledPolicy{}, ErrDenied
	}
	if network.Mode == NetworkRegistry {
		if !containsCapability(policy.Capabilities, CapabilityNetworkRegistry) || len(network.RegistryDomain) == 0 {
			return CompiledPolicy{}, ErrDenied
		}
		network.RegistryDomain = canonicalDomains(network.RegistryDomain)
		if len(network.RegistryDomain) == 0 {
			return CompiledPolicy{}, ErrDenied
		}
	}
	if network.Mode != NetworkNone && network.Mode != NetworkRegistry {
		return CompiledPolicy{}, ErrDenied
	}
	return CompiledPolicy{Version: policy.Version, Mounts: mounts, Network: network, Credentials: CompiledCredentials{Provider: policy.Credentials.Provider, Scopes: append([]string(nil), policy.Credentials.AllowedScopes...)}}, nil
}

func CompileExecution(policy ExecutionPolicy, spec ExecutionSpec) (CompiledExecution, error) {
	if spec.Backend != "apple-container" && spec.Backend != "lima" {
		return CompiledExecution{}, ErrDenied
	}
	if !safeRuntimePath(spec.CWD) || spec.Executable == "" || strings.ContainsAny(spec.Executable, "\x00\r\n") || isShell(spec.Executable) || !containsString(policy.AllowedExecutables, spec.Executable) {
		return CompiledExecution{}, ErrDenied
	}
	maxArgs := policy.MaxArguments
	if maxArgs <= 0 {
		maxArgs = 128
	}
	maxBytes := policy.MaxArgBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	if len(spec.Arguments) > maxArgs {
		return CompiledExecution{}, ErrDenied
	}
	bytes := 0
	args := append([]string(nil), spec.Arguments...)
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) {
			return CompiledExecution{}, ErrDenied
		}
		bytes += len(arg)
	}
	if bytes > maxBytes {
		return CompiledExecution{}, ErrDenied
	}
	return CompiledExecution{Backend: spec.Backend, Executable: spec.Executable, Arguments: args, CWD: spec.CWD}, nil
}

func validatePolicy(policy Policy) error {
	if policy.Version != PolicyVersion {
		return ErrDenied
	}
	seen := map[Capability]struct{}{}
	for _, capability := range policy.Capabilities {
		if capability == "" {
			return ErrDenied
		}
		if _, ok := seen[capability]; ok {
			return ErrDenied
		}
		seen[capability] = struct{}{}
	}
	if policy.Network.Mode == NetworkFull {
		return ErrDenied
	}
	return nil
}

func validateTarget(request Request, binding Binding) error {
	switch request.Capability {
	case CapabilityRepoRead, CapabilityRepoSearch:
		if request.Target != TargetLiveRepository || !validRelativePath(request.Path) {
			return ErrDenied
		}
	case CapabilityWorkspaceRead, CapabilityWorkspaceWrite, CapabilityExecute, CapabilityRuntimeCreate:
		if request.Target != TargetTaskWorkspace && request.Target != TargetRuntime {
			return ErrDenied
		}
		if request.Path != "" && !validRelativePath(request.Path) {
			return ErrDenied
		}
	case CapabilityIntegrate, CapabilityPublish:
		if request.Target != TargetLiveRepository || (request.Path != "" && !validRelativePath(request.Path)) {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	if sameOrWithin(binding.LiveRepository, binding.TaskWorkspace) || sameOrWithin(binding.TaskWorkspace, binding.LiveRepository) {
		return ErrDenied
	}
	return nil
}

func requiredConfirmation(capability Capability) ConfirmationClass {
	switch capability {
	case CapabilityIntegrate:
		return ConfirmationDestructive
	case CapabilityPublish:
		return ConfirmationPublication
	default:
		return ConfirmationNone
	}
}

func (e *Engine) consumeConfirmationLocked(sessionID, token string, class ConfirmationClass) error {
	confirmation, ok := e.confirmations[token]
	if !ok || confirmation.SessionID != sessionID || confirmation.Class != class || !confirmation.ExpiresAt.After(time.Now().UTC()) {
		return ErrDenied
	}
	delete(e.confirmations, token)
	return nil
}

func (e *Engine) validateTrustedRefLocked(repositoryID, ref string) error {
	enrollment, ok := e.enrollments[repositoryID]
	if !ok || ref == "" || digestString(ref) != enrollment.TrustedRefDigest {
		return ErrDenied
	}
	return nil
}

func (e *Engine) appendAuditLocked(event AuditEvent) {
	event.Detail = redact(event.Detail)
	e.audit = append(e.audit, event)
}

func containsCapability(capabilities []Capability, wanted Capability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func canonicalDomains(domains []string) []string {
	set := map[string]struct{}{}
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain != "" && !strings.ContainsAny(domain, "/\\\x00: ") {
			set[domain] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for domain := range set {
		result = append(result, domain)
	}
	sort.Strings(result)
	return result
}

func validID(value string) bool {
	return validOpaque(value) && !strings.Contains(value, "secret") && !strings.Contains(value, "token")
}

func validIdentity(value string) bool {
	if len(value) != 64 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validOpaque(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n/\\") {
		return false
	}
	return true
}

func validRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func safeAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func safeRuntimePath(path string) bool {
	return safeAbsolute(path) && (path == "/workspace" || strings.HasPrefix(path, "/workspace/")) && !strings.Contains(path, "..")
}

func sameOrWithin(parent, candidate string) bool {
	if !safeAbsolute(parent) || !safeAbsolute(candidate) {
		return true
	}
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}

func isShell(executable string) bool {
	base := strings.ToLower(filepath.Base(executable))
	return base == "sh" || base == "bash" || base == "zsh" || base == "fish" || base == "cmd" || base == "powershell"
}

func newOpaque(prefix string) (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data), nil
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func redact(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == 0 || r == '\r' {
			return -1
		}
		return r
	}, value)
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization: bearer ", "-----begin private key-----", "ghp_", "github_pat_", "sk-proj-"} {
		if strings.Contains(lower, marker) {
			return "[redacted]"
		}
	}
	if len(value) > 1024 {
		return value[:1024] + "…"
	}
	return value
}
