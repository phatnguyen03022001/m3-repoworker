// Package controlplane is the production composition root for RepoWorker.
// It owns one authenticated transport principal, wires every M3 subsystem, and
// exposes typed operations to the MCP adapter without exposing host shell
// capabilities.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	stdRuntime "runtime"
	"sync"
	"syscall"
	"time"

	"github.com/tienphat/m3-repoworker/internal/environment"
	"github.com/tienphat/m3-repoworker/internal/events"
	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/memory"
	"github.com/tienphat/m3-repoworker/internal/process"
	"github.com/tienphat/m3-repoworker/internal/publication"
	"github.com/tienphat/m3-repoworker/internal/repo"
	m3runtime "github.com/tienphat/m3-repoworker/internal/runtime"
	"github.com/tienphat/m3-repoworker/internal/scheduler"
	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/taskstate"
	"github.com/tienphat/m3-repoworker/internal/workspace"
)

var (
	ErrRejected = errors.New("control plane request rejected")
	ErrStale    = errors.New("control plane candidate is stale")
)

type Config struct {
	RepositoryRoot    string
	StateRoot         string
	Image             string
	PrincipalProvider security.PrincipalProvider
	Transport         security.TransportMetadata
	OperatorAuthority security.OperatorAuthority
}

type WorkspaceRecord struct {
	Generation workspace.Generation `json:"generation"`
	Lease      workspace.Lease      `json:"lease"`
}

type RuntimeRecord struct {
	Runtime m3runtime.Runtime `json:"runtime"`
}

type ProcessRecord struct {
	ID           string
	GenerationID string
	RuntimeID    string
	Process      *process.Process
}

type VerificationRecord struct {
	Plan   intelligence.VerificationPlan
	Result intelligence.VerificationResult
}

type Plane struct {
	Repo        *repo.Workspace
	Repository  *workspace.Repository
	Tasks       taskstate.StateStore
	Memory      *memory.Store
	Security    *security.Engine
	Operator    security.OperatorAuthority
	Processes   *process.Supervisor
	Runtimes    *m3runtime.Manager
	Scheduler   *scheduler.Scheduler
	Environment *environment.Manager
	Events      *events.Store
	Publication *publication.Adapter

	RepositoryID string
	FilesystemID string
	PrincipalID  string
	SessionID    string
	StateRoot    string

	mu            sync.Mutex
	loopWG        sync.WaitGroup
	loopCancels   map[string]context.CancelFunc
	session       security.Session
	trustedRef    string
	workspaces    map[string]WorkspaceRecord
	runtimes      map[string]m3runtime.Runtime
	processes     map[string]ProcessRecord
	verifications map[string]VerificationRecord
	environments  map[string]environment.Generation
	pending       map[string]security.ConfirmationBinding
	replay        *security.RequestReplayCache
	image         string
}

func Open(ctx context.Context, config Config) (*Plane, error) {
	if ctx == nil || config.RepositoryRoot == "" || config.StateRoot == "" || config.PrincipalProvider == nil || !filepath.IsAbs(config.RepositoryRoot) || !filepath.IsAbs(config.StateRoot) || filepath.Clean(config.RepositoryRoot) == filepath.Clean(config.StateRoot) {
		return nil, ErrRejected
	}
	if config.Image == "" {
		config.Image = "alpine:latest"
	}
	readRepo, err := repo.New(config.RepositoryRoot)
	if err != nil {
		return nil, ErrRejected
	}
	principal, err := config.PrincipalProvider.Authenticate(ctx, config.Transport)
	if err != nil {
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	isolated, err := workspace.OpenRepository(readRepo.StartupPath(), config.StateRoot)
	if err != nil {
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	tasks, err := taskstate.New(readRepo.StartupPath(), readRepo.RootIdentity(), config.StateRoot)
	if err != nil || tasks.RequireMain(ctx) != nil {
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	repositoryID := isolated.RootIdentity()
	filesystemID := isolated.SourceFilesystemIdentity()
	policy := defaultPolicy()
	engine, err := security.NewEngine(policy)
	if err != nil {
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	_, trustedRef, err := engine.EnrollRepository(ctx, principal.ID, repositoryID, filesystemID)
	if err != nil {
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	session, err := engine.OpenAuthenticatedSession(ctx, principal, repositoryID, 24*time.Hour)
	if err != nil {
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	memoryStore, err := memory.Open(filepath.Join(config.StateRoot, "memory.db"))
	if err != nil {
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	eventStore, err := events.Open(filepath.Join(config.StateRoot, "events.db"))
	if err != nil {
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	cache, err := environment.NewCache(filepath.Join(config.StateRoot, "cache"))
	if err != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	environments, err := environment.NewManager(filepath.Join(config.StateRoot, "environments"), cache, nil)
	if err != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	runtimes, err := m3runtime.NewManager(isolated, filepath.Join(config.StateRoot, "runtimes"), m3runtime.AppleContainerAdapter{}, m3runtime.LimaAdapter{})
	if err != nil || runtimes.Recover(ctx) != nil || isolated.Recover(ctx) != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	schedulerInstance, err := scheduler.New(scheduler.Config{Capacity: scheduler.Resources{CPU: stdRuntime.NumCPU(), MemoryBytes: 4 << 30}, MaxConcurrent: stdRuntime.NumCPU(), MaxQueued: 128, Classes: map[string]scheduler.JobClass{scheduler.ClassBuild: {Weight: 1}, scheduler.ClassTest: {Weight: 2}, scheduler.ClassVerify: {Weight: 2}, scheduler.ClassInteractive: {Weight: 1}}})
	if err != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	containerBinary, err := exec.LookPath("container")
	if err != nil || !filepath.IsAbs(containerBinary) {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	var plane *Plane
	starter := process.ContainerStarter{Binary: containerBinary, Resolve: func(resolveCtx context.Context, runtimeID string) (string, error) {
		if plane == nil {
			return "", ErrRejected
		}
		record, lookupErr := plane.Runtimes.Lookup(resolveCtx, runtimeID)
		if lookupErr != nil || record.State != m3runtime.StateRunning || record.Backend != "apple-container" {
			return "", ErrRejected
		}
		return record.ExternalID, nil
	}}
	processes, err := process.New(starter, filepath.Join(config.StateRoot, "process-spill"))
	if err != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	publicationAdapter, err := publication.New(publication.Gate{}, publication.OSRunner{}, func(snapshotCtx context.Context, root string) (string, error) {
		manifest, snapshotErr := workspace.SnapshotPath(snapshotCtx, root)
		if snapshotErr != nil {
			return "", ErrRejected
		}
		return manifest, nil
	})
	if err != nil {
		_ = eventStore.Close()
		_ = memoryStore.Close()
		_ = isolated.Close()
		_ = readRepo.Close()
		return nil, ErrRejected
	}
	plane = &Plane{Repo: readRepo, Repository: isolated, Tasks: tasks, Memory: memoryStore, Security: engine, Operator: config.OperatorAuthority, Processes: processes, Runtimes: runtimes, Scheduler: schedulerInstance, Environment: environments, Events: eventStore, Publication: publicationAdapter, RepositoryID: repositoryID, FilesystemID: filesystemID, PrincipalID: principal.ID, SessionID: session.ID, StateRoot: config.StateRoot, session: session, trustedRef: trustedRef, workspaces: map[string]WorkspaceRecord{}, runtimes: map[string]m3runtime.Runtime{}, processes: map[string]ProcessRecord{}, verifications: map[string]VerificationRecord{}, environments: map[string]environment.Generation{}, pending: map[string]security.ConfirmationBinding{}, replay: security.NewDefaultRequestReplayCache(), loopCancels: map[string]context.CancelFunc{}, image: config.Image}
	return plane, nil
}

func (p *Plane) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	for _, cancel := range p.loopCancels {
		cancel()
	}
	p.mu.Unlock()
	p.loopWG.Wait()
	if p.Events != nil {
		_ = p.Events.Close()
	}
	if p.Memory != nil {
		_ = p.Memory.Close()
	}
	if p.Repository != nil {
		_ = p.Repository.Close()
	}
	if p.Repo != nil {
		_ = p.Repo.Close()
	}
	return nil
}

func defaultPolicy() security.Policy {
	return security.Policy{Version: security.PolicyVersion, Capabilities: []security.Capability{security.CapabilityRepoRead, security.CapabilityRepoSearch, security.CapabilityWorkspaceRead, security.CapabilityWorkspaceWrite, security.CapabilityExecute, security.CapabilityProcessControl, security.CapabilityRuntimeCreate, security.CapabilityIntegrate, security.CapabilityPublish}, Mounts: []security.MountRule{{Source: security.MountTaskWorkspace, Target: "/workspace"}}, Network: security.NetworkPolicy{Mode: security.NetworkNone}, Execution: security.ExecutionPolicy{AllowedExecutables: []string{"go", "node", "npm", "pnpm", "yarn", "cargo", "rustc", "nx", "turbo", "bazel"}, MaxArgBytes: 8192, MaxArguments: 64}}
}

func (p *Plane) authorize(ctx context.Context, capability security.Capability, target security.TargetKind, path string, execution security.ExecutionSpec, confirmation string, generation workspace.Generation) error {
	return p.authorizeWithBinding(ctx, capability, target, path, execution, confirmation, security.ConfirmationBinding{}, generation)
}

func (p *Plane) authorizeWithBinding(ctx context.Context, capability security.Capability, target security.TargetKind, path string, execution security.ExecutionSpec, confirmation string, confirmationBinding security.ConfirmationBinding, generation workspace.Generation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p == nil {
		return ErrRejected
	}
	binding := security.Binding{RepositoryID: p.RepositoryID, FilesystemID: p.FilesystemID, LiveRepository: p.Repository.LiveRoot(), TaskWorkspace: generation.Path, WorkspaceID: generation.ID}
	request := security.Request{SessionID: p.session.ID, PrincipalID: p.PrincipalID, RepositoryID: p.RepositoryID, Nonce: p.session.Nonce, Capability: capability, Target: target, Path: path, TrustedIntegrationRef: p.trustedRef, ConfirmationToken: confirmation, ConfirmationBinding: confirmationBinding, Execution: execution}
	decision, err := p.Security.Authorize(ctx, request, binding)
	if err != nil || !decision.Allowed {
		return ErrRejected
	}
	p.session.Nonce = decision.NextNonce
	return nil
}

// AcceptMCPRequest records the request identity supplied in the MCP SDK's
// CallToolParams._meta. The SDK session handle is supplied by the adapter;
// authenticated transport, principal, and control-plane session bindings are
// taken from this plane and cannot be supplied by the caller.
func (p *Plane) AcceptMCPRequest(ctx context.Context, mcpSessionID, requestID string, sequence uint64) error {
	if p == nil || ctx == nil {
		return ErrRejected
	}
	p.mu.Lock()
	session := p.session
	replay := p.replay
	p.mu.Unlock()
	if replay == nil {
		return ErrRejected
	}
	if err := replay.Accept(session.TransportID, session.ID, session.PrincipalID, mcpSessionID, requestID, sequence); err != nil {
		return ErrRejected
	}
	return nil
}

func (p *Plane) IssueOperatorConfirmation(ctx context.Context, request security.OperatorConfirmationRequest) (security.Confirmation, error) {
	if p == nil {
		return security.Confirmation{}, ErrRejected
	}
	p.mu.Lock()
	operator := p.Operator
	expected, ok := p.pending[confirmationPendingKey(request.Class, request.Binding.GenerationID)]
	if operator == nil || !ok || expected != request.Binding || request.Binding.RepositoryID != p.RepositoryID || request.Binding.PrincipalID != p.PrincipalID || request.Binding.SessionID != p.session.ID {
		p.mu.Unlock()
		return security.Confirmation{}, ErrRejected
	}
	delete(p.pending, confirmationPendingKey(request.Class, request.Binding.GenerationID))
	p.mu.Unlock()
	confirmation, err := p.Security.IssueOperatorConfirmation(ctx, operator, request)
	if err != nil {
		return security.Confirmation{}, ErrRejected
	}
	return confirmation, nil
}

func confirmationPendingKey(class security.ConfirmationClass, generationID string) string {
	return string(class) + "\x00" + generationID
}

func (p *Plane) rememberConfirmationBinding(class security.ConfirmationClass, binding security.ConfirmationBinding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[string]security.ConfirmationBinding)
	}
	p.pending[confirmationPendingKey(class, binding.GenerationID)] = binding
}

func (p *Plane) IntegrationConfirmationBinding(ctx context.Context, generationID string) (security.ConfirmationBinding, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil || !p.hasVerifiedCandidate(ctx, record) {
		return security.ConfirmationBinding{}, ErrStale
	}
	plan, err := p.Repository.BuildIntegrationPlan(ctx, record.Generation, record.Lease)
	if err != nil {
		return security.ConfirmationBinding{}, ErrRejected
	}
	binding := security.ConfirmationBinding{Action: digestText("repository.integrate:" + plan.PlanDigest), RepositoryID: p.RepositoryID, PrincipalID: p.PrincipalID, SessionID: p.session.ID, GenerationID: generationID, FencingGeneration: record.Lease.FencingGeneration, CandidateSnapshot: record.Generation.CandidateSnapshot, PlanDigest: plan.PlanDigest}
	p.rememberConfirmationBinding(security.ConfirmationDestructive, binding)
	return binding, nil
}

func (p *Plane) CreateWorkspace(ctx context.Context, taskID string) (WorkspaceRecord, error) {
	if taskID == "" {
		return WorkspaceRecord{}, ErrRejected
	}
	generation, err := p.Repository.Materialize(ctx)
	if err != nil {
		return WorkspaceRecord{}, ErrRejected
	}
	lease, err := p.Repository.AcquireLease(ctx, generation.ID, p.PrincipalID, 30*time.Minute)
	if err != nil {
		return WorkspaceRecord{}, ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceWrite, security.TargetTaskWorkspace, "", security.ExecutionSpec{}, "", generation); err != nil {
		_ = p.Repository.ReleaseLease(ctx, lease)
		_ = p.Repository.DiscardGeneration(ctx, generation.ID)
		return WorkspaceRecord{}, ErrRejected
	}
	record := WorkspaceRecord{Generation: generation, Lease: lease}
	p.mu.Lock()
	p.workspaces[generation.ID] = record
	p.mu.Unlock()
	return record, nil
}

func (p *Plane) WorkspaceStatus(ctx context.Context, generationID string) (WorkspaceRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx == nil {
		return WorkspaceRecord{}, ErrRejected
	}
	record, ok := p.workspaces[generationID]
	if !ok {
		// Rehydration is intentionally performed before any caller can mutate
		// the generation. A new lease creates a new fencing epoch.
		p.mu.Unlock()
		rehydrated, rehydrateErr := p.ensureWorkspace(ctx, generationID)
		p.mu.Lock()
		if rehydrateErr != nil {
			return WorkspaceRecord{}, ErrRejected
		}
		return rehydrated, nil
	}
	return record, nil
}

func (p *Plane) ensureWorkspace(ctx context.Context, generationID string) (WorkspaceRecord, error) {
	p.mu.Lock()
	if record, ok := p.workspaces[generationID]; ok {
		p.mu.Unlock()
		return record, nil
	}
	p.mu.Unlock()
	generation, err := p.Repository.LoadGeneration(ctx, generationID)
	if err != nil {
		return WorkspaceRecord{}, ErrRejected
	}
	lease, err := p.Repository.AcquireLease(ctx, generationID, p.PrincipalID, 30*time.Minute)
	if err != nil {
		return WorkspaceRecord{}, ErrRejected
	}
	record := WorkspaceRecord{Generation: generation, Lease: lease}
	p.mu.Lock()
	p.workspaces[generationID] = record
	p.mu.Unlock()
	return record, nil
}

func (p *Plane) DiscardWorkspace(ctx context.Context, generationID string) error {
	p.mu.Lock()
	record, ok := p.workspaces[generationID]
	if ok {
		delete(p.workspaces, generationID)
	}
	p.mu.Unlock()
	if !ok {
		return ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceWrite, security.TargetTaskWorkspace, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return ErrRejected
	}
	if err := p.Repository.ReleaseLease(ctx, record.Lease); err != nil {
		return ErrRejected
	}
	return p.Repository.DiscardGeneration(ctx, generationID)
}

func (p *Plane) IntegrationPlan(ctx context.Context, generationID string) (workspace.IntegrationPlan, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return workspace.IntegrationPlan{}, err
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceRead, security.TargetTaskWorkspace, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return workspace.IntegrationPlan{}, ErrRejected
	}
	plan, err := p.Repository.BuildIntegrationPlan(ctx, record.Generation, record.Lease)
	if err != nil {
		return workspace.IntegrationPlan{}, ErrRejected
	}
	// Planning is read-only for the repository, but it creates the short-lived
	// pending record that the independent operator channel must approve. The
	// record is only available when the candidate is still verified.
	if p.hasVerifiedCandidate(ctx, record) {
		p.rememberConfirmationBinding(security.ConfirmationDestructive, security.ConfirmationBinding{Action: digestText("repository.integrate:" + plan.PlanDigest), RepositoryID: p.RepositoryID, PrincipalID: p.PrincipalID, SessionID: p.session.ID, GenerationID: generationID, FencingGeneration: record.Lease.FencingGeneration, CandidateSnapshot: record.Generation.CandidateSnapshot, PlanDigest: plan.PlanDigest})
	}
	return plan, nil
}

func (p *Plane) Integrate(ctx context.Context, generationID, confirmation string) (workspace.IntegrationJournal, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return workspace.IntegrationJournal{}, err
	}
	if !p.hasVerifiedCandidate(ctx, record) {
		return workspace.IntegrationJournal{}, ErrStale
	}
	plan, err := p.Repository.BuildIntegrationPlan(ctx, record.Generation, record.Lease)
	if err != nil {
		return workspace.IntegrationJournal{}, ErrRejected
	}
	binding := security.ConfirmationBinding{Action: digestText("repository.integrate:" + plan.PlanDigest), RepositoryID: p.RepositoryID, PrincipalID: p.PrincipalID, SessionID: p.session.ID, GenerationID: generationID, FencingGeneration: record.Lease.FencingGeneration, CandidateSnapshot: record.Generation.CandidateSnapshot, PlanDigest: plan.PlanDigest}
	if err := p.authorizeWithBinding(ctx, security.CapabilityIntegrate, security.TargetLiveRepository, "", security.ExecutionSpec{}, confirmation, binding, record.Generation); err != nil {
		return workspace.IntegrationJournal{}, ErrRejected
	}
	return p.Repository.ApplyIntegration(ctx, plan, record.Lease)
}

func (p *Plane) hasVerifiedCandidate(ctx context.Context, record WorkspaceRecord) bool {
	current, err := candidateSnapshot(ctx, record.Generation.Path)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, verification := range p.verifications {
		if intelligence.ValidResult(verification.Result, verification.Plan, current, verification.Plan.EnvironmentID, verification.Plan.PolicyVersion) {
			return true
		}
	}
	return false
}

func (p *Plane) RuntimeCreate(ctx context.Context, generationID, image, backend string, cpu int, memoryBytes int64) (m3runtime.Runtime, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return m3runtime.Runtime{}, err
	}
	if backend == "" {
		backend = "apple-container"
	}
	if image == "" {
		image = p.image
	}
	if cpu == 0 {
		cpu = 2
	}
	if memoryBytes == 0 {
		memoryBytes = 512 << 20
	}
	if err := p.authorize(ctx, security.CapabilityRuntimeCreate, security.TargetRuntime, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	if err := p.Repository.ReserveRuntime(ctx, record.Lease, p.PrincipalID); err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	created, err := p.Runtimes.Create(ctx, m3runtime.RuntimeSpec{TaskID: generationID, Generation: record.Generation, Lease: record.Lease, WorkspacePath: record.Generation.Path, LiveRepositoryPath: p.Repository.LiveRoot(), Image: image, CPU: cpu, MemoryBytes: memoryBytes, Network: security.NetworkNone}, backend)
	if err != nil {
		_ = p.Repository.ReleaseRuntime(ctx, record.Lease, p.PrincipalID)
		return m3runtime.Runtime{}, ErrRejected
	}
	p.mu.Lock()
	p.runtimes[generationID] = created
	p.mu.Unlock()
	return created, nil
}

func (p *Plane) RuntimeStart(ctx context.Context, generationID string) (m3runtime.Runtime, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return m3runtime.Runtime{}, err
	}
	if err := p.authorize(ctx, security.CapabilityRuntimeCreate, security.TargetRuntime, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	started, err := p.Runtimes.Start(ctx, generationID, record.Lease)
	if err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	p.mu.Lock()
	p.runtimes[generationID] = started
	p.mu.Unlock()
	return started, nil
}

func (p *Plane) RuntimeStop(ctx context.Context, generationID string) (m3runtime.Runtime, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return m3runtime.Runtime{}, err
	}
	if err := p.authorize(ctx, security.CapabilityRuntimeCreate, security.TargetRuntime, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	stopped, err := p.Runtimes.Stop(ctx, generationID, record.Lease)
	if err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	_ = p.Repository.ReleaseRuntime(ctx, record.Lease, p.PrincipalID)
	p.mu.Lock()
	p.runtimes[generationID] = stopped
	p.mu.Unlock()
	return stopped, nil
}

func (p *Plane) RuntimeStatus(ctx context.Context, generationID string) (m3runtime.Runtime, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return m3runtime.Runtime{}, err
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceRead, security.TargetRuntime, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return m3runtime.Runtime{}, ErrRejected
	}
	return p.Runtimes.Status(ctx, generationID)
}

func (p *Plane) ProcessStart(ctx context.Context, generationID, runtimeID, executable string, args []string, cwd string, timeout time.Duration) (string, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return "", err
	}
	runtimeRecord, err := p.Runtimes.Lookup(ctx, runtimeID)
	if err != nil || runtimeRecord.GenerationID != generationID || runtimeRecord.State != m3runtime.StateRunning {
		return "", ErrRejected
	}
	if cwd == "" {
		cwd = "/workspace"
	}
	execution := security.ExecutionSpec{Backend: runtimeRecord.Backend, Executable: executable, Arguments: append([]string(nil), args...), CWD: cwd}
	if err := p.authorize(ctx, security.CapabilityExecute, security.TargetRuntime, "", execution, "", record.Generation); err != nil {
		return "", ErrRejected
	}
	handle, err := p.Processes.Start(ctx, process.ProcessSpec{TaskID: generationID, WorkspaceGeneration: generationID, LeaseGeneration: record.Lease.FencingGeneration, RuntimeID: runtimeID, Execution: security.CompiledExecution{Backend: execution.Backend, Executable: execution.Executable, Arguments: execution.Arguments, CWD: execution.CWD}, Timeout: timeout})
	if err != nil {
		return "", ErrRejected
	}
	id, err := randomID("proc_")
	if err != nil {
		handle.Cancel()
		return "", ErrRejected
	}
	p.mu.Lock()
	p.processes[id] = ProcessRecord{ID: id, GenerationID: generationID, RuntimeID: runtimeID, Process: handle}
	p.mu.Unlock()
	return id, nil
}

func (p *Plane) ProcessRead(ctx context.Context, processID string, after process.Cursor, limit int) ([]process.Chunk, error) {
	record, generation, err := p.processContext(ctx, processID)
	if err != nil {
		return nil, ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceRead, security.TargetRuntime, "", security.ExecutionSpec{}, "", generation.Generation); err != nil {
		return nil, ErrRejected
	}
	return record.Process.Read(after, limit)
}
func (p *Plane) ProcessCancel(ctx context.Context, processID string) error {
	record, generation, err := p.processContext(ctx, processID)
	if err != nil {
		return ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityProcessControl, security.TargetRuntime, "", security.ExecutionSpec{}, "", generation.Generation); err != nil {
		return ErrRejected
	}
	record.Process.Cancel()
	return nil
}
func (p *Plane) ProcessSignal(ctx context.Context, processID string, signal int) error {
	record, generation, err := p.processContext(ctx, processID)
	if err != nil {
		return ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityProcessControl, security.TargetRuntime, "", security.ExecutionSpec{}, "", generation.Generation); err != nil {
		return ErrRejected
	}
	value, err := signalValue(signal)
	if err != nil {
		return err
	}
	return record.Process.Signal(value)
}
func (p *Plane) ProcessWait(ctx context.Context, processID string) (process.Outcome, error) {
	record, generation, err := p.processContext(ctx, processID)
	if err != nil {
		return process.Outcome{}, ErrRejected
	}
	if err := p.authorize(ctx, security.CapabilityProcessControl, security.TargetRuntime, "", security.ExecutionSpec{}, "", generation.Generation); err != nil {
		return process.Outcome{}, ErrRejected
	}
	return record.Process.Wait(ctx)
}

func (p *Plane) processContext(ctx context.Context, processID string) (ProcessRecord, WorkspaceRecord, error) {
	if ctx == nil {
		return ProcessRecord{}, WorkspaceRecord{}, ErrRejected
	}
	p.mu.Lock()
	record, ok := p.processes[processID]
	p.mu.Unlock()
	if !ok {
		return ProcessRecord{}, WorkspaceRecord{}, ErrRejected
	}
	generation, err := p.WorkspaceStatus(ctx, record.GenerationID)
	if err != nil || generation.Generation.ID != record.GenerationID {
		return ProcessRecord{}, WorkspaceRecord{}, ErrRejected
	}
	return record, generation, nil
}

func (p *Plane) PlanVerification(ctx context.Context, generationID string, target intelligence.Target) (intelligence.VerificationPlan, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return intelligence.VerificationPlan{}, err
	}
	if err := p.authorize(ctx, security.CapabilityWorkspaceRead, security.TargetTaskWorkspace, "", security.ExecutionSpec{}, "", record.Generation); err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	info, err := intelligence.Detect(ctx, record.Generation.Path)
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	toolchain, err := environment.Detect(ctx, record.Generation.Path)
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	snapshot, err := candidateSnapshot(ctx, record.Generation.Path)
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	lockfileDigest, err := environment.HashLockfiles(ctx, record.Generation.Path)
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	environmentGeneration, err := p.Environment.Create(ctx, environment.EnvironmentSpec{RepositoryID: p.RepositoryID, WorkspaceID: record.Generation.ID, Platform: stdRuntime.GOOS + "/" + stdRuntime.GOARCH, Toolchain: toolchain, LockfileDigest: lockfileDigest, PolicyVersion: security.PolicyVersion, Image: p.image})
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	plan, err := intelligence.BuildPlan(ctx, info, p.RepositoryID, snapshot, environmentGeneration.Identity, security.PolicyVersion, target)
	if err != nil {
		return intelligence.VerificationPlan{}, ErrRejected
	}
	p.mu.Lock()
	p.verifications[plan.PlanDigest] = VerificationRecord{Plan: plan}
	p.environments[environmentGeneration.Identity] = environmentGeneration
	p.mu.Unlock()
	return plan, nil
}

func (p *Plane) EventsCreate(ctx context.Context, taskID, generationID, environmentID string) (events.Run, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return events.Run{}, err
	}
	id, err := randomID("run_")
	if err != nil {
		return events.Run{}, ErrRejected
	}
	now := time.Now().UTC()
	run := events.Run{ID: id, TaskID: taskID, RepositoryID: p.RepositoryID, GenerationID: generationID, EnvironmentID: environmentID, PolicyVersion: security.PolicyVersion, CandidateSnapshot: record.Generation.CandidateSnapshot, Status: "created", CreatedAt: now, UpdatedAt: now}
	if err := p.Events.CreateRun(ctx, run); err != nil {
		return events.Run{}, ErrRejected
	}
	return run, nil
}

func (p *Plane) AppendEvent(ctx context.Context, runID, eventType, payload string) (events.Event, error) {
	if containsSecret(eventType) || containsSecret(payload) {
		return events.Event{}, ErrRejected
	}
	return p.Events.AppendEvent(ctx, runID, eventType, payload)
}
func (p *Plane) ListEvents(ctx context.Context, runID string, after int64, limit int) ([]events.Event, error) {
	return p.Events.ListEvents(ctx, runID, after, limit)
}

func candidateSnapshot(ctx context.Context, root string) (string, error) {
	manifest, err := workspace.SnapshotPath(ctx, root)
	if err != nil {
		return "", ErrRejected
	}
	return manifest, nil
}
func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
func signalValue(signal int) (os.Signal, error) {
	switch syscall.Signal(signal) {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		return syscall.Signal(signal), nil
	default:
		return nil, ErrRejected
	}
}
