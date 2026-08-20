package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tienphat/m3-repoworker/internal/controlplane"
	"github.com/tienphat/m3-repoworker/internal/events"
	"github.com/tienphat/m3-repoworker/internal/intelligence"
	"github.com/tienphat/m3-repoworker/internal/process"
	"github.com/tienphat/m3-repoworker/internal/publication"
	"github.com/tienphat/m3-repoworker/internal/repo"
	m3runtime "github.com/tienphat/m3-repoworker/internal/runtime"
	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/verify"
	"github.com/tienphat/m3-repoworker/internal/workspace"
)

type WorkspaceCreateInput struct {
	TaskID string `json:"task_id" jsonschema:"caller-owned task identifier; it is never used as a filesystem path"`
}

type WorkspaceIDInput struct {
	GenerationID string `json:"generation_id" jsonschema:"RepoWorker workspace generation identifier"`
}

type WorkspaceIntegrateInput struct {
	GenerationID      string `json:"generation_id"`
	ConfirmationToken string `json:"confirmation_token,omitempty" jsonschema:"one-time integration confirmation token"`
}

type RuntimeCreateInput struct {
	GenerationID string `json:"generation_id"`
	Image        string `json:"image,omitempty" jsonschema:"pinned runtime image reference"`
	Backend      string `json:"backend,omitempty" jsonschema:"apple-container; Lima is not the production default"`
	CPU          int    `json:"cpu,omitempty"`
	MemoryBytes  int64  `json:"memory_bytes,omitempty"`
	Network      string `json:"network,omitempty" jsonschema:"none; registry/full are rejected until domain-filtered support exists"`
}

type ProcessRunInput struct {
	GenerationID string            `json:"generation_id"`
	RuntimeID    string            `json:"runtime_id"`
	Executable   string            `json:"executable" jsonschema:"typed executable name; sh/bash/zsh are allowed only inside the isolated Apple container"`
	Arguments    []string          `json:"arguments,omitempty" jsonschema:"bounded argv entries"`
	CWD          string            `json:"cwd,omitempty" jsonschema:"workspace-relative container path, default /workspace"`
	Environment  map[string]string `json:"environment,omitempty" jsonschema:"bounded non-secret key/value overrides; host environment is never inherited"`
	TimeoutSecs  int               `json:"timeout_seconds,omitempty" jsonschema:"bounded timeout; zero selects the server default"`
}

type ProcessIDInput struct {
	ProcessID string `json:"process_id"`
}

type ProcessReadInput struct {
	ProcessID string         `json:"process_id"`
	After     process.Cursor `json:"after,omitempty"`
	Limit     int            `json:"limit,omitempty"`
}

type ProcessSignalInput struct {
	ProcessID string `json:"process_id"`
	Signal    int    `json:"signal" jsonschema:"supported termination signal number"`
}

type VerificationPlanInput struct {
	GenerationID string `json:"generation_id"`
	TargetName   string `json:"target_name,omitempty"`
	Affected     bool   `json:"affected,omitempty"`
}

type VerificationRunInput struct {
	PlanDigest   string `json:"plan_digest"`
	GenerationID string `json:"generation_id"`
	RuntimeID    string `json:"runtime_id"`
}

type VerificationStatusInput struct {
	PlanDigest string `json:"plan_digest"`
}

type RunCreateInput struct {
	TaskID        string `json:"task_id"`
	GenerationID  string `json:"generation_id"`
	EnvironmentID string `json:"environment_id"`
}

type EventAppendInput struct {
	RunID     string `json:"run_id"`
	EventType string `json:"event_type"`
	Payload   string `json:"payload,omitempty"`
}

type EventListInput struct {
	RunID string `json:"run_id"`
	After int64  `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// MCP output types deliberately omit host paths, spill files, and container
// implementation identifiers. Callers operate on opaque IDs and the stable
// /workspace mount instead of learning the control plane's state layout.
type WorkspaceOutput struct {
	GenerationID      string          `json:"generation_id"`
	RepositoryID      string          `json:"repository_id"`
	CandidateSnapshot string          `json:"candidate_snapshot"`
	FencingGeneration uint64          `json:"fencing_generation"`
	State             string          `json:"state"`
	CreatedAt         time.Time       `json:"created_at"`
	Lease             workspace.Lease `json:"lease"`
}

type RuntimeOutput struct {
	ID              string          `json:"id"`
	Backend         string          `json:"backend"`
	Network         string          `json:"network"`
	TaskID          string          `json:"task_id"`
	GenerationID    string          `json:"generation_id"`
	LeaseGeneration uint64          `json:"lease_generation"`
	Identity        string          `json:"identity"`
	State           m3runtime.State `json:"state"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type OutcomeOutput struct {
	ExitCode  int  `json:"exit_code"`
	TimedOut  bool `json:"timed_out"`
	Canceled  bool `json:"canceled"`
	Truncated bool `json:"truncated"`
}

type VerificationCommandOutput struct {
	Ecosystem  intelligence.Ecosystem `json:"ecosystem"`
	Executable string                 `json:"executable"`
	Arguments  []string               `json:"arguments"`
	Workdir    string                 `json:"workdir"`
}

type VerificationPlanOutput struct {
	RepositoryID      string                      `json:"repository_id"`
	CandidateSnapshot string                      `json:"candidate_snapshot"`
	EnvironmentID     string                      `json:"environment_id"`
	PolicyVersion     string                      `json:"policy_version"`
	Target            intelligence.Target         `json:"target"`
	Commands          []VerificationCommandOutput `json:"commands"`
	PlanDigest        string                      `json:"plan_digest"`
}

type VerificationOutput struct {
	Plan   VerificationPlanOutput          `json:"plan"`
	Result intelligence.VerificationResult `json:"result"`
}

func publicWorkspace(record controlplane.WorkspaceRecord) WorkspaceOutput {
	return WorkspaceOutput{GenerationID: record.Generation.ID, RepositoryID: record.Generation.RepositoryID, CandidateSnapshot: record.Generation.CandidateSnapshot, FencingGeneration: record.Generation.FencingGeneration, State: record.Generation.State, CreatedAt: record.Generation.CreatedAt, Lease: record.Lease}
}

func publicRuntime(record m3runtime.Runtime) RuntimeOutput {
	return RuntimeOutput{ID: record.ID, Backend: record.Backend, Network: string(record.Network), TaskID: record.TaskID, GenerationID: record.GenerationID, LeaseGeneration: record.LeaseGeneration, Identity: record.Identity, State: record.State, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func publicVerificationPlan(plan intelligence.VerificationPlan) VerificationPlanOutput {
	output := VerificationPlanOutput{RepositoryID: plan.RepositoryID, CandidateSnapshot: plan.CandidateSnapshot, EnvironmentID: plan.EnvironmentID, PolicyVersion: plan.PolicyVersion, Target: plan.Target, PlanDigest: plan.PlanDigest}
	output.Commands = make([]VerificationCommandOutput, 0, len(plan.Commands))
	for _, command := range plan.Commands {
		output.Commands = append(output.Commands, VerificationCommandOutput{Ecosystem: command.Ecosystem, Executable: command.Executable, Arguments: append([]string(nil), command.Arguments...), Workdir: "/workspace"})
	}
	return output
}

type LoopStartInput struct {
	TaskID       string `json:"task_id"`
	GenerationID string `json:"generation_id"`
	RuntimeID    string `json:"runtime_id"`
	PlanDigest   string `json:"plan_digest"`
	Patch        string `json:"patch,omitempty" jsonschema:"optional strict single-file candidate patch; never a shell command"`
}

type LoopRunInput struct {
	RunID string `json:"run_id"`
}

type PublicationInput struct {
	GenerationID      string           `json:"generation_id"`
	PlanDigest        string           `json:"plan_digest"`
	Kind              publication.Kind `json:"kind"`
	Remote            string           `json:"remote,omitempty"`
	Ref               string           `json:"ref,omitempty"`
	ExpectedRemoteRef string           `json:"expected_remote_ref,omitempty"`
	Base              string           `json:"base,omitempty"`
	Head              string           `json:"head,omitempty"`
	Title             string           `json:"title,omitempty"`
	Body              string           `json:"body,omitempty"`
	Workflow          string           `json:"workflow,omitempty"`
	ConfirmationToken string           `json:"confirmation_token,omitempty"`
}

func (input PublicationInput) request() publication.Request {
	return publication.Request{Kind: input.Kind, ConfirmationToken: input.ConfirmationToken, Remote: input.Remote, Ref: input.Ref, ExpectedRemoteRef: input.ExpectedRemoteRef, Base: input.Base, Head: input.Head, Title: input.Title, Body: input.Body, Workflow: input.Workflow}
}

func m3Tool(name, title, description string, readOnly, idempotent, destructive, openWorld bool) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    readOnly,
			IdempotentHint:  idempotent,
			DestructiveHint: boolPtr(destructive),
			OpenWorldHint:   boolPtr(openWorld),
		},
	}
}

func newControlPlaneServer(plane *controlplane.Plane) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "m3-repoworker", Version: "0.4.0"}, nil)
	installMCPReplayGuard(server, plane)

	// The production server keeps repository access read-only. Candidate edits
	// are deliberately absent here; they must be performed by an isolated loop
	// authority in a TaskWorkspace generation.
	mcp.AddTool(server, m3Tool("repo_status", "Repository status", "Return authenticated control-plane and repository health.", true, true, false, false), func(_ context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, RepoStatusOutput, error) {
		return nil, RepoStatusOutput{Status: "ok", RepositoryID: plane.RepositoryID, PrincipalID: plane.PrincipalID, SessionID: plane.SessionID}, nil
	})
	mcp.AddTool(server, m3Tool("repo_git_status", "Read Git status", "Return typed read-only Git status bound to the opened repository authority.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, _ RepoSnapshotInput) (*mcp.CallToolResult, repo.GitStatus, error) {
		status, err := plane.Repo.GitStatus(ctx)
		if err != nil {
			return nil, repo.GitStatus{}, safeToolError(err)
		}
		return nil, status, nil
	})
	mcp.AddTool(server, m3Tool("repo_read", "Read repository file", "Read one permitted UTF-8 file from the live repository without mutation.", true, true, false, false), func(_ context.Context, _ *mcp.CallToolRequest, input RepoReadInput) (*mcp.CallToolResult, RepoReadOutput, error) {
		path, content, err := plane.Repo.Read(input.Path)
		if err != nil {
			return nil, RepoReadOutput{}, safeToolError(err)
		}
		return nil, RepoReadOutput{Path: path, Content: content}, nil
	})
	mcp.AddTool(server, m3Tool("repo_search", "Search repository", "Perform a bounded literal search of permitted repository files.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input RepoSearchInput) (*mcp.CallToolResult, RepoSearchOutput, error) {
		result, err := plane.Repo.Search(ctx, input.Query, input.Path)
		if err != nil {
			return nil, RepoSearchOutput{}, safeToolError(err)
		}
		return nil, RepoSearchOutput{Matches: result.Matches, Truncated: result.Truncated}, nil
	})
	mcp.AddTool(server, m3Tool("repo_snapshot", "Snapshot repository", "Return a deterministic read-only snapshot of the live repository.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, _ RepoSnapshotInput) (*mcp.CallToolResult, repo.SnapshotManifest, error) {
		manifest, err := plane.Repo.Snapshot(ctx)
		if err != nil {
			return nil, repo.SnapshotManifest{}, safeToolError(err)
		}
		return nil, manifest, nil
	})
	mcp.AddTool(server, m3Tool("repo_verify", "Run fixed repository verification", "Run one allow-listed repository verification preset with bounded output; no client-supplied shell command is accepted.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input RepoVerifyInput) (*mcp.CallToolResult, RepoVerifyOutput, error) {
		if err := plane.Tasks.RequireMain(ctx); err != nil {
			return nil, RepoVerifyOutput{}, safeToolError(err)
		}
		preset, ok := verify.VerificationPreset(input.Check)
		if !ok {
			return nil, RepoVerifyOutput{}, errRequestRejected
		}
		root, err := plane.Repo.DuplicateRoot()
		if err != nil {
			return nil, RepoVerifyOutput{}, safeToolError(err)
		}
		defer root.Close()
		outcome, err := verify.Run(ctx, root, preset, []string{plane.Repo.StartupPath(), plane.StateRoot})
		if err != nil {
			return nil, RepoVerifyOutput{}, safeToolError(err)
		}
		return nil, RepoVerifyOutput{Check: input.Check, Passed: outcome.ExitCode == 0 && !outcome.TimedOut, ExitCode: outcome.ExitCode, TimedOut: outcome.TimedOut, FailureStage: string(outcome.FailureStage), Diagnostic: outcome.Diagnostic, Truncated: outcome.Truncated}, nil
	})

	mcp.AddTool(server, m3Tool("workspace_create", "Create task workspace", "Materialize and lease an isolated candidate workspace generation.", false, false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceCreateInput) (*mcp.CallToolResult, WorkspaceOutput, error) {
		record, err := plane.CreateWorkspace(ctx, input.TaskID)
		if err != nil {
			return nil, WorkspaceOutput{}, safeToolError(err)
		}
		return nil, publicWorkspace(record), nil
	})
	mcp.AddTool(server, m3Tool("workspace_status", "Read workspace status", "Read the generation and fencing lease for an isolated task workspace.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, WorkspaceOutput, error) {
		record, err := plane.WorkspaceStatus(ctx, input.GenerationID)
		if err != nil {
			return nil, WorkspaceOutput{}, safeToolError(err)
		}
		return nil, publicWorkspace(record), nil
	})
	mcp.AddTool(server, m3Tool("workspace_discard", "Discard task workspace", "Release the workspace lease and discard one isolated candidate generation.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, StatusOutput, error) {
		if err := plane.DiscardWorkspace(ctx, input.GenerationID); err != nil {
			return nil, StatusOutput{}, safeToolError(err)
		}
		return nil, StatusOutput{Status: "discarded"}, nil
	})
	mcp.AddTool(server, m3Tool("workspace_integration_plan", "Plan workspace integration", "Build a bounded integration journal from a verified candidate workspace without mutating the live repository.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, workspace.IntegrationPlan, error) {
		plan, err := plane.IntegrationPlan(ctx, input.GenerationID)
		if err != nil {
			return nil, workspace.IntegrationPlan{}, safeToolError(err)
		}
		return nil, plan, nil
	})
	mcp.AddTool(server, m3Tool("workspace_integrate", "Integrate verified workspace", "Apply a recovery-safe candidate integration to the live repository only with a scoped confirmation token.", false, false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIntegrateInput) (*mcp.CallToolResult, workspace.IntegrationJournal, error) {
		journal, err := plane.Integrate(ctx, input.GenerationID, input.ConfirmationToken)
		if err != nil {
			return nil, workspace.IntegrationJournal{}, safeToolError(err)
		}
		return nil, journal, nil
	})

	mcp.AddTool(server, m3Tool("runtime_create", "Create isolated runtime", "Create an Apple container bound to one leased workspace generation.", false, false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input RuntimeCreateInput) (*mcp.CallToolResult, RuntimeOutput, error) {
		network, err := parseRuntimeNetwork(input.Network)
		if err != nil {
			return nil, RuntimeOutput{}, errRequestRejected
		}
		runtimeRecord, err := plane.RuntimeCreate(ctx, input.GenerationID, input.Image, input.Backend, input.CPU, input.MemoryBytes, network)
		if err != nil {
			return nil, RuntimeOutput{}, safeToolError(err)
		}
		return nil, publicRuntime(runtimeRecord), nil
	})
	mcp.AddTool(server, m3Tool("runtime_start", "Start isolated runtime", "Start the Apple container bound to the leased workspace.", false, false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, RuntimeOutput, error) {
		runtimeRecord, err := plane.RuntimeStart(ctx, input.GenerationID)
		if err != nil {
			return nil, RuntimeOutput{}, safeToolError(err)
		}
		return nil, publicRuntime(runtimeRecord), nil
	})
	mcp.AddTool(server, m3Tool("runtime_stop", "Stop isolated runtime", "Stop the Apple container and release its runtime reservation.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, RuntimeOutput, error) {
		runtimeRecord, err := plane.RuntimeStop(ctx, input.GenerationID)
		if err != nil {
			return nil, RuntimeOutput{}, safeToolError(err)
		}
		return nil, publicRuntime(runtimeRecord), nil
	})
	mcp.AddTool(server, m3Tool("runtime_status", "Read runtime status", "Read the persisted state of an isolated runtime.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input WorkspaceIDInput) (*mcp.CallToolResult, RuntimeOutput, error) {
		runtimeRecord, err := plane.RuntimeStatus(ctx, input.GenerationID)
		if err != nil {
			return nil, RuntimeOutput{}, safeToolError(err)
		}
		return nil, publicRuntime(runtimeRecord), nil
	})

	mcp.AddTool(server, m3Tool("process_run", "Run bounded process", "Run one typed executable inside the selected isolated runtime with bounded argv, controlled environment, cwd, timeout, and output. Shells are confined to the Apple container.", false, false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input ProcessRunInput) (*mcp.CallToolResult, map[string]string, error) {
		if input.TimeoutSecs < 0 || input.TimeoutSecs > 3600 {
			return nil, nil, errRequestRejected
		}
		// process_run is asynchronous: the SDK request context is commonly
		// canceled as soon as this response is returned. The process receives
		// its own explicit timeout and remains controllable through the typed
		// process_cancel/signal operations.
		id, err := plane.ProcessStartWithEnvironment(context.WithoutCancel(ctx), input.GenerationID, input.RuntimeID, input.Executable, input.Arguments, input.CWD, time.Duration(input.TimeoutSecs)*time.Second, input.Environment)
		if err != nil {
			return nil, nil, safeToolError(err)
		}
		return nil, map[string]string{"process_id": id}, nil
	})
	mcp.AddTool(server, m3Tool("process_read", "Read process output", "Read bounded stdout/stderr chunks after a cursor.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input ProcessReadInput) (*mcp.CallToolResult, []process.Chunk, error) {
		chunks, err := plane.ProcessRead(ctx, input.ProcessID, input.After, input.Limit)
		if err != nil {
			return nil, nil, safeToolError(err)
		}
		return nil, chunks, nil
	})
	mcp.AddTool(server, m3Tool("process_signal", "Signal process", "Send one of the explicitly supported termination signals to a supervised process group.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input ProcessSignalInput) (*mcp.CallToolResult, StatusOutput, error) {
		if err := plane.ProcessSignal(ctx, input.ProcessID, input.Signal); err != nil {
			return nil, StatusOutput{}, safeToolError(err)
		}
		return nil, StatusOutput{Status: "signaled"}, nil
	})
	mcp.AddTool(server, m3Tool("process_cancel", "Cancel process", "Cancel a supervised process and its process group.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input ProcessIDInput) (*mcp.CallToolResult, StatusOutput, error) {
		if err := plane.ProcessCancel(ctx, input.ProcessID); err != nil {
			return nil, StatusOutput{}, safeToolError(err)
		}
		return nil, StatusOutput{Status: "canceled"}, nil
	})
	mcp.AddTool(server, m3Tool("process_wait", "Wait for process", "Wait for a supervised process to finish and return its bounded outcome.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input ProcessIDInput) (*mcp.CallToolResult, OutcomeOutput, error) {
		outcome, err := plane.ProcessWait(ctx, input.ProcessID)
		if err != nil {
			return nil, OutcomeOutput{}, safeToolError(err)
		}
		return nil, OutcomeOutput{ExitCode: outcome.ExitCode, TimedOut: outcome.TimedOut, Canceled: outcome.Canceled, Truncated: outcome.Truncated}, nil
	})

	mcp.AddTool(server, m3Tool("verification_plan", "Plan bound verification", "Create a verification plan bound to the candidate snapshot, environment, policy, and repository.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input VerificationPlanInput) (*mcp.CallToolResult, VerificationPlanOutput, error) {
		plan, err := plane.PlanVerification(ctx, input.GenerationID, intelligence.Target{Name: input.TargetName, Affected: input.Affected})
		if err != nil {
			return nil, VerificationPlanOutput{}, safeToolError(err)
		}
		return nil, publicVerificationPlan(plan), nil
	})
	mcp.AddTool(server, m3Tool("verification_run", "Run bound verification", "Execute the persisted verification plan inside the selected isolated runtime and recheck its snapshot binding.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input VerificationRunInput) (*mcp.CallToolResult, intelligence.VerificationResult, error) {
		result, err := plane.RunVerification(ctx, input.PlanDigest, input.GenerationID, input.RuntimeID)
		if err != nil {
			return nil, intelligence.VerificationResult{}, safeToolError(err)
		}
		return nil, result, nil
	})
	mcp.AddTool(server, m3Tool("verification_status", "Read verification status", "Read the persisted verification plan and result binding.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input VerificationStatusInput) (*mcp.CallToolResult, VerificationOutput, error) {
		record, err := plane.VerificationStatus(ctx, input.PlanDigest)
		if err != nil {
			return nil, VerificationOutput{}, safeToolError(err)
		}
		return nil, VerificationOutput{Plan: publicVerificationPlan(record.Plan), Result: record.Result}, nil
	})
	mcp.AddTool(server, m3Tool("run_create", "Create durable run", "Create a durable event/checkpoint run bound to a task workspace and environment.", false, false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input RunCreateInput) (*mcp.CallToolResult, events.Run, error) {
		run, err := plane.EventsCreate(ctx, input.TaskID, input.GenerationID, input.EnvironmentID)
		if err != nil {
			return nil, events.Run{}, safeToolError(err)
		}
		return nil, run, nil
	})
	mcp.AddTool(server, m3Tool("run_event_append", "Append run event", "Append one bounded durable event to a run.", false, false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input EventAppendInput) (*mcp.CallToolResult, events.Event, error) {
		event, err := plane.AppendEvent(ctx, input.RunID, input.EventType, input.Payload)
		if err != nil {
			return nil, events.Event{}, safeToolError(err)
		}
		return nil, event, nil
	})
	mcp.AddTool(server, m3Tool("run_events", "Read run events", "Read durable run events after a bounded sequence cursor.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input EventListInput) (*mcp.CallToolResult, []events.Event, error) {
		if input.Limit == 0 {
			input.Limit = 100
		}
		result, err := plane.ListEvents(ctx, input.RunID, input.After, input.Limit)
		if err != nil {
			return nil, nil, safeToolError(err)
		}
		return nil, result, nil
	})
	mcp.AddTool(server, m3Tool("loop_start", "Start autonomous loop", "Start the bounded persisted autonomous candidate loop using a previously planned verification binding.", false, false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input LoopStartInput) (*mcp.CallToolResult, events.Run, error) {
		record, err := plane.VerificationStatus(ctx, input.PlanDigest)
		if err != nil {
			return nil, events.Run{}, safeToolError(err)
		}
		run, err := plane.StartLoop(ctx, input.TaskID, input.GenerationID, input.RuntimeID, input.Patch, record.Plan)
		if err != nil {
			return nil, events.Run{}, safeToolError(err)
		}
		return nil, run, nil
	})
	mcp.AddTool(server, m3Tool("loop_resume", "Resume autonomous loop", "Resume a durable autonomous loop from its event/checkpoint state after restart recovery.", false, true, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, input LoopRunInput) (*mcp.CallToolResult, events.Run, error) {
		run, err := plane.ResumeLoop(ctx, input.RunID)
		if err != nil {
			return nil, events.Run{}, safeToolError(err)
		}
		return nil, run, nil
	})
	mcp.AddTool(server, m3Tool("loop_status", "Read autonomous loop status", "Read the durable run and latest loop state after a cursor-based event replay.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input LoopRunInput) (*mcp.CallToolResult, controlplane.LoopStatus, error) {
		status, err := plane.LoopStatus(ctx, input.RunID)
		if err != nil {
			return nil, controlplane.LoopStatus{}, safeToolError(err)
		}
		return nil, status, nil
	})

	mcp.AddTool(server, m3Tool("publication_plan", "Plan verified publication", "Create a plan-only publication result from a still-verified candidate.", true, true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, input PublicationInput) (*mcp.CallToolResult, publication.Result, error) {
		result, err := plane.PublicationPlan(ctx, input.GenerationID, input.PlanDigest, input.request())
		if err != nil {
			return nil, publication.Result{}, safeToolError(err)
		}
		return nil, result, nil
	})
	mcp.AddTool(server, m3Tool("publication_execute", "Execute verified publication", "Execute a verified local checkpoint or external publication only with a scoped confirmation token and rechecked candidate binding.", false, false, true, true), func(ctx context.Context, _ *mcp.CallToolRequest, input PublicationInput) (*mcp.CallToolResult, publication.Result, error) {
		result, err := plane.PublicationExecute(ctx, input.GenerationID, input.PlanDigest, input.request())
		if err != nil {
			return nil, publication.Result{}, safeToolError(err)
		}
		return nil, result, nil
	})

	return server
}

var mutatingMCPTools = map[string]struct{}{
	"workspace_create":    {},
	"workspace_discard":   {},
	"workspace_integrate": {},
	"runtime_create":      {},
	"runtime_start":       {},
	"runtime_stop":        {},
	"process_run":         {},
	"process_signal":      {},
	"process_cancel":      {},
	"verification_run":    {},
	"run_create":          {},
	"run_event_append":    {},
	"loop_start":          {},
	"loop_resume":         {},
	"publication_execute": {},
}

func installMCPReplayGuard(server *mcp.Server, plane *controlplane.Plane) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, request)
			}
			call, ok := request.(*mcp.CallToolRequest)
			if !ok || call.Params == nil {
				return next(ctx, method, request)
			}
			if _, mutating := mutatingMCPTools[call.Params.Name]; !mutating {
				return next(ctx, method, request)
			}
			requestID, sequence, ok := mcpReplayMetadata(call.Params.Meta)
			if !ok {
				return nil, errRequestRejected
			}
			if call.Session == nil {
				return nil, errRequestRejected
			}
			mcpSessionID := call.Session.ID()
			if mcpSessionID == "" {
				// Stdio has no protocol session header. The SDK session object is
				// still the actual logical transport session for this connection.
				mcpSessionID = fmt.Sprintf("stdio:%p", call.Session)
			}
			if err := plane.AcceptMCPRequest(ctx, mcpSessionID, requestID, sequence); err != nil {
				return nil, errRequestRejected
			}
			return next(ctx, method, request)
		}
	})
}

func parseRuntimeNetwork(value string) (security.NetworkMode, error) {
	if value == "" {
		return security.NetworkNone, nil
	}
	mode := security.NetworkMode(value)
	if mode != security.NetworkNone && mode != security.NetworkRegistry && mode != security.NetworkFull {
		return "", errRequestRejected
	}
	return mode, nil
}

func mcpReplayMetadata(meta mcp.Meta) (string, uint64, bool) {
	requestID, ok := meta[mcpSecurityRequestIDKey].(string)
	if !ok || len(requestID) == 0 || len(requestID) > 128 || strings.ContainsAny(requestID, "\x00\r\n/\\") {
		return "", 0, false
	}
	var sequence uint64
	switch value := meta[mcpSecurityRequestSequenceKey].(type) {
	case int:
		if value > 0 {
			sequence = uint64(value)
		}
	case int64:
		if value > 0 {
			sequence = uint64(value)
		}
	case uint64:
		sequence = value
	case float64:
		if value > 0 && value <= math.MaxInt64 && math.Trunc(value) == value {
			sequence = uint64(value)
		}
	case json.Number:
		parsed, err := value.Int64()
		if err == nil && parsed > 0 {
			sequence = uint64(parsed)
		}
	}
	return requestID, sequence, sequence != 0
}

const (
	mcpSecurityRequestIDKey       = security.MCPRequestIDMetaKey
	mcpSecurityRequestSequenceKey = security.MCPRequestSequenceMetaKey
)
