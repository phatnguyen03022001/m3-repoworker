package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/tienphat/m3-repoworker/internal/events"
	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/loop"
	"github.com/tienphat/m3-repoworker/internal/process"
	"github.com/tienphat/m3-repoworker/internal/publication"
	"github.com/tienphat/m3-repoworker/internal/repo"
	m3runtime "github.com/tienphat/m3-repoworker/internal/runtime"
	"github.com/tienphat/m3-repoworker/internal/security"
)

type LoopStatus struct {
	Run   events.Run `json:"run"`
	State loop.State `json:"state"`
}

type loopConfig struct {
	GenerationID string              `json:"generation_id"`
	RuntimeID    string              `json:"runtime_id"`
	Patch        string              `json:"patch,omitempty"`
	Target       intelligence.Target `json:"target"`
}

func (p *Plane) RunVerification(ctx context.Context, planDigest, generationID, runtimeID string) (intelligence.VerificationResult, error) {
	if ctx == nil || planDigest == "" || generationID == "" || runtimeID == "" {
		return intelligence.VerificationResult{}, ErrRejected
	}
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return intelligence.VerificationResult{}, ErrRejected
	}
	p.mu.Lock()
	verification, ok := p.verifications[planDigest]
	p.mu.Unlock()
	if !ok || verification.Plan.RepositoryID != p.RepositoryID || verification.Plan.CandidateSnapshot != record.Generation.CandidateSnapshot {
		return intelligence.VerificationResult{}, ErrStale
	}
	runtimeRecord, err := p.Runtimes.Lookup(ctx, runtimeID)
	if err != nil || runtimeRecord.GenerationID != generationID || runtimeRecord.State != "RUNNING" {
		return intelligence.VerificationResult{}, ErrRejected
	}
	current, err := candidateSnapshot(ctx, record.Generation.Path)
	if err != nil {
		return intelligence.VerificationResult{}, ErrRejected
	}
	if current != verification.Plan.CandidateSnapshot {
		return intelligence.VerificationResult{}, ErrStale
	}
	runner := &verificationProcessRunner{plane: p, generationID: generationID, runtimeID: runtimeID, workspaceRoot: record.Generation.Path}
	result, verifyErr := intelligence.Verify(ctx, verification.Plan, func(snapshotCtx context.Context) (string, error) {
		return candidateSnapshot(snapshotCtx, record.Generation.Path)
	}, runner)
	if verifyErr != nil {
		if after, snapshotErr := candidateSnapshot(ctx, record.Generation.Path); snapshotErr == nil && after != verification.Plan.CandidateSnapshot {
			return intelligence.VerificationResult{}, ErrStale
		}
		return intelligence.VerificationResult{}, ErrRejected
	}
	p.mu.Lock()
	verification.Result = result
	p.verifications[planDigest] = verification
	p.mu.Unlock()
	return result, nil
}

func (p *Plane) VerificationStatus(ctx context.Context, planDigest string) (VerificationRecord, error) {
	if ctx == nil {
		return VerificationRecord{}, ErrRejected
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.verifications[planDigest]
	if !ok {
		return VerificationRecord{}, ErrRejected
	}
	return record, nil
}

func (p *Plane) PublicationPlan(ctx context.Context, generationID, planDigest string, request publication.Request) (publication.Result, error) {
	candidate, _, err := p.publicationCandidate(ctx, generationID, planDigest)
	if err != nil {
		return publication.Result{}, err
	}
	request.Mode = publication.ModePlan
	request.DryRun = false
	request.ConfirmationToken = ""
	return p.Publication.Publish(ctx, candidate, request)
}

func (p *Plane) PublicationExecute(ctx context.Context, generationID, planDigest string, request publication.Request) (publication.Result, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return publication.Result{}, ErrRejected
	}
	verification, err := p.VerificationStatus(ctx, planDigest)
	if err != nil || verification.Plan.CandidateSnapshot != record.Generation.CandidateSnapshot || !verification.Result.Passed {
		return publication.Result{}, ErrStale
	}
	if request.ConfirmationToken == "" {
		return publication.Result{}, ErrRejected
	}
	candidate, _, err := p.publicationCandidate(ctx, generationID, planDigest)
	if err != nil {
		return publication.Result{}, err
	}
	binding := p.publicationConfirmationBinding(generationID, planDigest, record, request)
	if err := p.authorizeWithBinding(ctx, security.CapabilityPublish, security.TargetLiveRepository, "", security.ExecutionSpec{}, request.ConfirmationToken, binding, record.Generation); err != nil {
		return publication.Result{}, ErrRejected
	}
	request.Mode = publication.ModeExecute
	request.DryRun = false
	adapter, err := p.Publication.WithGate(publication.Gate{Enabled: true, AllowLocalMutation: request.Kind == publication.KindGitCheckpoint || request.Kind == publication.KindJJCheckpoint, AllowExternalMutation: request.Kind != publication.KindGitCheckpoint && request.Kind != publication.KindJJCheckpoint, ConfirmationToken: request.ConfirmationToken})
	if err != nil {
		return publication.Result{}, ErrRejected
	}
	return adapter.Publish(ctx, candidate, request)
}

func (p *Plane) PublicationConfirmationBinding(ctx context.Context, generationID, planDigest string, request publication.Request) (security.ConfirmationBinding, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return security.ConfirmationBinding{}, ErrRejected
	}
	if _, _, err := p.publicationCandidate(ctx, generationID, planDigest); err != nil {
		return security.ConfirmationBinding{}, err
	}
	return p.publicationConfirmationBinding(generationID, planDigest, record, request), nil
}

func (p *Plane) publicationConfirmationBinding(generationID, planDigest string, record WorkspaceRecord, request publication.Request) security.ConfirmationBinding {
	request.ConfirmationToken = ""
	payload, _ := json.Marshal(request)
	return security.ConfirmationBinding{Action: digestText("repository.publish:" + string(payload)), RepositoryID: p.RepositoryID, PrincipalID: p.PrincipalID, SessionID: p.session.ID, GenerationID: generationID, FencingGeneration: record.Lease.FencingGeneration, CandidateSnapshot: record.Generation.CandidateSnapshot, PlanDigest: planDigest}
}

func (p *Plane) publicationCandidate(ctx context.Context, generationID, planDigest string) (publication.Candidate, VerificationRecord, error) {
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return publication.Candidate{}, VerificationRecord{}, ErrRejected
	}
	verification, err := p.VerificationStatus(ctx, planDigest)
	if err != nil || verification.Plan.RepositoryID != p.RepositoryID || !verification.Result.Passed {
		return publication.Candidate{}, VerificationRecord{}, ErrStale
	}
	current, err := candidateSnapshot(ctx, record.Generation.Path)
	if err != nil || !intelligence.ValidResult(verification.Result, verification.Plan, current, verification.Plan.EnvironmentID, verification.Plan.PolicyVersion) {
		return publication.Candidate{}, VerificationRecord{}, ErrStale
	}
	return publication.Candidate{RepositoryRoot: record.Generation.Path, CandidateSnapshot: current, VerifiedSnapshot: verification.Result.CandidateSnapshot, EnvironmentID: verification.Result.EnvironmentID, PolicyVersion: verification.Result.PolicyVersion, Verified: true}, verification, nil
}

type verificationProcessRunner struct {
	plane         *Plane
	generationID  string
	runtimeID     string
	workspaceRoot string
}

func (r *verificationProcessRunner) Run(ctx context.Context, command intelligence.Command) (int, string) {
	if ctx == nil || r == nil || r.plane == nil || !filepath.IsAbs(command.Workdir) || !pathWithin(r.workspaceRoot, command.Workdir) {
		return -1, "verification command rejected"
	}
	relative, err := filepath.Rel(r.workspaceRoot, command.Workdir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return -1, "verification workdir rejected"
	}
	cwd := "/workspace"
	if relative != "." {
		cwd += "/" + filepath.ToSlash(relative)
	}
	processID, err := r.plane.ProcessStart(ctx, r.generationID, r.runtimeID, command.Executable, command.Arguments, cwd, 0)
	if err != nil {
		return -1, "supervised process rejected"
	}
	outcome, err := r.plane.ProcessWait(ctx, processID)
	if err != nil {
		return -1, "supervised process wait failed"
	}
	chunks, _ := r.plane.ProcessRead(ctx, processID, 0, 64)
	diagnostic := boundedDiagnostic(chunks)
	if outcome.TimedOut {
		return outcome.ExitCode, "verification timed out: " + diagnostic
	}
	if outcome.Canceled {
		return outcome.ExitCode, "verification canceled: " + diagnostic
	}
	return outcome.ExitCode, diagnostic
}

func boundedDiagnostic(chunks []process.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		if builder.Len() >= 4096 {
			break
		}
		data := chunk.Data
		remaining := 4096 - builder.Len()
		if len(data) > remaining {
			data = data[:remaining]
		}
		builder.WriteString(data)
	}
	return builder.String()
}

func (p *Plane) StartLoop(ctx context.Context, taskID, generationID, runtimeID, patch string, plan intelligence.VerificationPlan) (events.Run, error) {
	if ctx == nil || taskID == "" || runtimeID == "" || plan.PlanDigest == "" || plan.RepositoryID != p.RepositoryID {
		return events.Run{}, ErrRejected
	}
	record, err := p.WorkspaceStatus(ctx, generationID)
	if err != nil {
		return events.Run{}, ErrRejected
	}
	current, err := candidateSnapshot(ctx, record.Generation.Path)
	if err != nil || current != plan.CandidateSnapshot {
		return events.Run{}, ErrStale
	}
	if containsSecret(patch) {
		return events.Run{}, ErrRejected
	}
	p.mu.Lock()
	p.verifications[plan.PlanDigest] = VerificationRecord{Plan: plan}
	p.mu.Unlock()
	run, err := p.EventsCreate(ctx, taskID, generationID, plan.EnvironmentID)
	if err != nil {
		return events.Run{}, ErrRejected
	}
	config := loopConfig{GenerationID: generationID, RuntimeID: runtimeID, Patch: patch, Target: plan.Target}
	payload, err := json.Marshal(config)
	if err != nil {
		return events.Run{}, ErrRejected
	}
	if _, err := p.Events.AppendEvent(ctx, run.ID, "loop.config", string(payload)); err != nil {
		return events.Run{}, ErrRejected
	}
	if err := p.Events.UpdateRunStatus(ctx, run.ID, "running"); err != nil {
		return events.Run{}, ErrRejected
	}
	run.Status = "running"
	p.launchLoop(ctx, run, config, plan)
	return run, nil
}

func (p *Plane) ResumeLoop(ctx context.Context, runID string) (events.Run, error) {
	if ctx == nil || runID == "" {
		return events.Run{}, ErrRejected
	}
	run, err := p.Events.GetRun(ctx, runID)
	if err != nil {
		return events.Run{}, ErrRejected
	}
	if run.Status == "completed" {
		return run, nil
	}
	if run.Status == "failed" || run.Status == "stopped" {
		return events.Run{}, ErrRejected
	}
	page, err := p.Events.ListEvents(ctx, runID, 0, 1000)
	if err != nil {
		return events.Run{}, ErrRejected
	}
	var config loopConfig
	for _, event := range page {
		if event.Type == "loop.config" && json.Unmarshal([]byte(event.Payload), &config) != nil {
			return events.Run{}, ErrRejected
		}
	}
	if config.GenerationID == "" || config.RuntimeID == "" {
		return events.Run{}, ErrRejected
	}
	record, err := p.WorkspaceStatus(ctx, config.GenerationID)
	if err != nil || record.Generation.RepositoryID != p.RepositoryID || record.Generation.CandidateSnapshot != run.CandidateSnapshot {
		return events.Run{}, ErrStale
	}
	previousRuntime, err := p.Runtimes.Lookup(ctx, config.RuntimeID)
	if err != nil || previousRuntime.GenerationID != config.GenerationID {
		return events.Run{}, ErrRejected
	}
	if previousRuntime.State != m3runtime.StateRunning || previousRuntime.LeaseGeneration != record.Lease.FencingGeneration {
		if previousRuntime.State != m3runtime.StateStopped || previousRuntime.Image == "" {
			return events.Run{}, ErrRejected
		}
		created, createErr := p.RuntimeCreate(ctx, config.GenerationID, previousRuntime.Image, previousRuntime.Backend, 2, 512<<20)
		if createErr != nil {
			return events.Run{}, ErrRejected
		}
		started, startErr := p.RuntimeStart(ctx, config.GenerationID)
		if startErr != nil {
			return events.Run{}, ErrRejected
		}
		config.RuntimeID = started.ID
		if created.ID != started.ID {
			return events.Run{}, ErrRejected
		}
		payload, marshalErr := json.Marshal(config)
		if marshalErr != nil {
			return events.Run{}, ErrRejected
		}
		if _, appendErr := p.Events.AppendEvent(ctx, run.ID, "loop.config", string(payload)); appendErr != nil {
			return events.Run{}, ErrRejected
		}
	}
	plan, err := p.PlanVerification(ctx, config.GenerationID, config.Target)
	if err != nil || plan.CandidateSnapshot != run.CandidateSnapshot || plan.EnvironmentID != run.EnvironmentID {
		return events.Run{}, ErrStale
	}
	if err := p.Events.UpdateRunStatus(ctx, run.ID, "running"); err != nil {
		return events.Run{}, ErrRejected
	}
	run.Status = "running"
	p.launchLoop(ctx, run, config, plan)
	return run, nil
}

func (p *Plane) LoopStatus(ctx context.Context, runID string) (LoopStatus, error) {
	if ctx == nil || runID == "" {
		return LoopStatus{}, ErrRejected
	}
	run, err := p.Events.GetRun(ctx, runID)
	if err != nil {
		return LoopStatus{}, ErrRejected
	}
	page, err := p.Events.ListEvents(ctx, runID, 0, 1000)
	if err != nil {
		return LoopStatus{}, ErrRejected
	}
	state := loop.State{Phase: loop.PhaseInspect}
	for _, event := range page {
		if event.Type != "loop.state" {
			continue
		}
		var candidate loop.State
		if json.Unmarshal([]byte(event.Payload), &candidate) != nil {
			return LoopStatus{}, ErrRejected
		}
		state = candidate
	}
	return LoopStatus{Run: run, State: state}, nil
}

func (p *Plane) launchLoop(ctx context.Context, run events.Run, config loopConfig, plan intelligence.VerificationPlan) {
	loopContext, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.loopCancels == nil {
		p.loopCancels = map[string]context.CancelFunc{}
	}
	p.loopCancels[run.ID] = cancel
	p.loopWG.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.loopWG.Done()
		defer func() {
			p.mu.Lock()
			delete(p.loopCancels, run.ID)
			p.mu.Unlock()
		}()
		binding := loop.Binding{RepositoryID: plan.RepositoryID, CandidateSnapshot: plan.CandidateSnapshot, EnvironmentID: plan.EnvironmentID, PolicyVersion: plan.PolicyVersion}
		authority := &loopAuthority{plane: p, generationID: config.GenerationID, runtimeID: config.RuntimeID, patch: config.Patch, target: plan.Target, verificationPlan: plan}
		controller, err := loop.New(p.Events, loopModel{binding: binding, patch: config.Patch}, authority, 2)
		if err != nil {
			return
		}
		if loopErr := func() error {
			_, err := controller.Run(loopContext, loop.Request{RunID: run.ID, Binding: binding})
			return err
		}(); loopErr != nil && !errors.Is(loopErr, context.Canceled) && loopContext.Err() == nil {
			_, _ = p.Events.AppendEvent(context.Background(), run.ID, "loop.error", "autonomous loop stopped")
			_ = p.Events.UpdateRunStatus(context.Background(), run.ID, "failed")
		}
	}()
}

type loopModel struct {
	binding loop.Binding
	patch   string
}

func (m loopModel) Inspect(context.Context, loop.Binding) (string, error) {
	return "execute the bounded candidate verification workflow", nil
}

func (m loopModel) Plan(context.Context, loop.Binding, string) (loop.Plan, error) {
	return m.plan(), nil
}

func (m loopModel) Diagnose(context.Context, loop.Binding, loop.Failure, []string) (loop.Plan, error) {
	return m.plan(), nil
}

func (m loopModel) plan() loop.Plan {
	return loop.Plan{Binding: m.binding, Commands: []loop.Action{{ID: "preflight-verification", Kind: "verification", Fingerprint: digestText("preflight:" + m.binding.CandidateSnapshot)}}, Patch: loop.Action{ID: "candidate-patch", Kind: "candidate_patch", Fingerprint: digestText(m.patch)}, TargetedTest: loop.Action{ID: "targeted-verification", Kind: "verification", Fingerprint: digestText("targeted:" + m.binding.CandidateSnapshot)}, FullVerify: loop.Action{ID: "full-verification", Kind: "verification", Fingerprint: digestText("full:" + m.binding.CandidateSnapshot)}}
}

type loopAuthority struct {
	plane            *Plane
	generationID     string
	runtimeID        string
	patch            string
	target           intelligence.Target
	verificationPlan intelligence.VerificationPlan
}

func (a *loopAuthority) ParallelCommands(ctx context.Context, _ loop.Binding, actions []loop.Action) error {
	if len(actions) == 0 {
		return ErrRejected
	}
	_, err := a.plane.RunVerification(ctx, a.verificationPlan.PlanDigest, a.generationID, a.runtimeID)
	return err
}

func (a *loopAuthority) PatchCandidate(ctx context.Context, _ loop.Binding, action loop.Action) error {
	if action.Kind != "candidate_patch" || a.patch == "" {
		return nil
	}
	record, err := a.plane.WorkspaceStatus(ctx, a.generationID)
	if err != nil || a.plane.Repository.AssertGeneration(ctx, record.Generation, record.Lease) != nil {
		return ErrRejected
	}
	candidate, err := repo.New(record.Generation.Path)
	if err != nil {
		return ErrRejected
	}
	defer candidate.Close()
	if _, err := candidate.ApplyPatch(a.patch); err != nil {
		return ErrRejected
	}
	refreshed, err := a.plane.Repository.RefreshGeneration(ctx, record.Generation, record.Lease)
	if err != nil {
		return ErrRejected
	}
	a.plane.mu.Lock()
	a.plane.workspaces[a.generationID] = WorkspaceRecord{Generation: refreshed, Lease: record.Lease}
	a.plane.mu.Unlock()
	return nil
}

func (a *loopAuthority) RefreshBinding(ctx context.Context, _ loop.Binding) (loop.Binding, error) {
	plan, err := a.plane.PlanVerification(ctx, a.generationID, a.target)
	if err != nil {
		return loop.Binding{}, err
	}
	a.verificationPlan = plan
	return loop.Binding{RepositoryID: plan.RepositoryID, CandidateSnapshot: plan.CandidateSnapshot, EnvironmentID: plan.EnvironmentID, PolicyVersion: plan.PolicyVersion}, nil
}

func (a *loopAuthority) TargetedTest(ctx context.Context, _ loop.Binding, _ loop.Action) error {
	result, err := a.plane.RunVerification(ctx, a.verificationPlan.PlanDigest, a.generationID, a.runtimeID)
	if err != nil {
		return err
	}
	if !result.Passed {
		return ErrRejected
	}
	return nil
}

func (a *loopAuthority) FullVerify(ctx context.Context, binding loop.Binding, action loop.Action) error {
	return a.TargetedTest(ctx, binding, action)
}

func (a *loopAuthority) Checkpoint(ctx context.Context, binding loop.Binding) error {
	record, err := a.plane.WorkspaceStatus(ctx, a.generationID)
	if err != nil || record.Generation.CandidateSnapshot != binding.CandidateSnapshot {
		return ErrStale
	}
	return a.plane.Repository.AssertGeneration(ctx, record.Generation, record.Lease)
}

func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func containsSecret(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization:", "bearer ", "-----begin private key-----", "ghp_", "github_pat_", "access_token", "private_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
