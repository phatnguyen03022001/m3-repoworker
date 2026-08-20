// Package loop implements the bounded, persisted autonomous coding loop.
// Model output is a proposal; the local Authority remains the execution and
// snapshot authority.
package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tienphat/m3-repoworker/internal/events"
)

var (
	ErrRejected        = errors.New("autonomous loop request rejected")
	ErrHumanCheckpoint = errors.New("autonomous loop requires human checkpoint")
	ErrRetryExhausted  = errors.New("autonomous loop retry budget exhausted")
)

type Phase string

const (
	PhaseInspect         Phase = "inspect"
	PhaseHypothesis      Phase = "hypothesis"
	PhasePlan            Phase = "plan"
	PhaseParallel        Phase = "parallel_commands"
	PhasePatch           Phase = "patch_candidate"
	PhaseTargetedTest    Phase = "targeted_test"
	PhaseDiagnoseRetry   Phase = "diagnose_retry"
	PhaseFullVerify      Phase = "full_verify"
	PhaseCheckpoint      Phase = "checkpoint"
	PhaseCompleted       Phase = "completed"
	PhaseFailed          Phase = "failed"
	PhaseHumanCheckpoint Phase = "human_checkpoint"
)

type Binding struct {
	RepositoryID      string `json:"repository_id"`
	CandidateSnapshot string `json:"candidate_snapshot"`
	EnvironmentID     string `json:"environment_id"`
	PolicyVersion     string `json:"policy_version"`
}

type Request struct {
	RunID   string
	Binding Binding
}

type Action struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
}

type Plan struct {
	Binding      Binding  `json:"binding"`
	Commands     []Action `json:"commands"`
	Patch        Action   `json:"patch"`
	TargetedTest Action   `json:"targeted_test"`
	FullVerify   Action   `json:"full_verify"`
	Destructive  bool     `json:"destructive"`
	Ambiguous    bool     `json:"ambiguous"`
}

type Failure struct {
	Phase      Phase  `json:"phase"`
	Action     Action `json:"action"`
	Class      string `json:"class"`
	Diagnostic string `json:"diagnostic"`
}

type State struct {
	Phase         Phase    `json:"phase"`
	Binding       Binding  `json:"binding"`
	Hypothesis    string   `json:"hypothesis,omitempty"`
	Plan          Plan     `json:"plan"`
	LastFailure   Failure  `json:"last_failure"`
	RetryCount    int      `json:"retry_count"`
	FailedActions []string `json:"failed_actions"`
	CheckpointID  string   `json:"checkpoint_id,omitempty"`
}

type Model interface {
	Inspect(context.Context, Binding) (string, error)
	Plan(context.Context, Binding, string) (Plan, error)
	Diagnose(context.Context, Binding, Failure, []string) (Plan, error)
}

type Authority interface {
	ParallelCommands(context.Context, Binding, []Action) error
	PatchCandidate(context.Context, Binding, Action) error
	TargetedTest(context.Context, Binding, Action) error
	FullVerify(context.Context, Binding, Action) error
	Checkpoint(context.Context, Binding) error
}

// BindingRefresher is implemented by authorities whose candidate mutation
// advances the snapshot. The loop then persists the new binding before any
// targeted or full verification runs, preventing a stale plan from being
// treated as verified.
type BindingRefresher interface {
	RefreshBinding(context.Context, Binding) (Binding, error)
}

type Controller struct {
	store      *events.Store
	model      Model
	authority  Authority
	maxRetries int
	now        func() time.Time
}

func New(store *events.Store, model Model, authority Authority, maxRetries int) (*Controller, error) {
	if store == nil || model == nil || authority == nil || maxRetries <= 0 || maxRetries > 10 {
		return nil, ErrRejected
	}
	return &Controller{store: store, model: model, authority: authority, maxRetries: maxRetries, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (c *Controller) Run(ctx context.Context, request Request) (State, error) {
	if ctx == nil || !validRequest(request) {
		return State{}, fmt.Errorf("loop request validation: %w", ErrRejected)
	}
	run, err := c.store.GetRun(ctx, request.RunID)
	if err != nil || run.RepositoryID != request.Binding.RepositoryID || run.CandidateSnapshot != request.Binding.CandidateSnapshot || run.EnvironmentID != request.Binding.EnvironmentID || run.PolicyVersion != request.Binding.PolicyVersion {
		return State{}, fmt.Errorf("loop run binding: %w", ErrRejected)
	}
	state, err := c.load(ctx, request)
	if err != nil {
		return State{}, fmt.Errorf("loop state load: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		switch state.Phase {
		case PhaseInspect:
			hypothesis, err := c.model.Inspect(ctx, request.Binding)
			if err != nil {
				return c.fail(ctx, request, state, PhaseInspect, Action{Kind: "inspect"}, "model", err)
			}
			if hypothesis == "" || len(hypothesis) > 4096 {
				return State{}, ErrRejected
			}
			state.Hypothesis = hypothesis
			state.Phase = PhaseHypothesis
		case PhaseHypothesis:
			state.Phase = PhasePlan
		case PhasePlan:
			plan, err := c.model.Plan(ctx, request.Binding, state.Hypothesis)
			if err != nil {
				return c.fail(ctx, request, state, PhasePlan, Action{Kind: "plan"}, "model", err)
			}
			if !validPlan(plan, request.Binding) || plan.Destructive || plan.Ambiguous {
				state.Phase = PhaseHumanCheckpoint
				if err := c.persist(ctx, request.RunID, state); err != nil {
					return State{}, err
				}
				return state, ErrHumanCheckpoint
			}
			state.Plan = plan
			state.Phase = PhaseParallel
		case PhaseParallel:
			if err := c.authority.ParallelCommands(ctx, request.Binding, state.Plan.Commands); err != nil {
				state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhaseParallel, Action: firstAction(state.Plan.Commands, "parallel"), Class: classify(err), Diagnostic: safeDiagnostic(err)})
				if err != nil {
					return state, err
				}
				break
			}
			state.Phase = PhasePatch
		case PhasePatch:
			if err := c.authority.PatchCandidate(ctx, request.Binding, state.Plan.Patch); err != nil {
				state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhasePatch, Action: state.Plan.Patch, Class: classify(err), Diagnostic: safeDiagnostic(err)})
				if err != nil {
					return state, err
				}
				break
			}
			if refresher, ok := c.authority.(BindingRefresher); ok {
				binding, err := refresher.RefreshBinding(ctx, request.Binding)
				if err != nil || !validBinding(binding) {
					state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhasePatch, Action: state.Plan.Patch, Class: "binding", Diagnostic: safeDiagnostic(err)})
					if err != nil {
						return state, err
					}
					break
				}
				request.Binding = binding
				state.Binding = binding
				state.Plan.Binding = binding
				if err := c.store.UpdateRunBinding(ctx, request.RunID, binding.CandidateSnapshot, binding.EnvironmentID, binding.PolicyVersion); err != nil {
					return State{}, err
				}
			}
			state.Phase = PhaseTargetedTest
		case PhaseTargetedTest:
			if err := c.authority.TargetedTest(ctx, request.Binding, state.Plan.TargetedTest); err != nil {
				state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhaseTargetedTest, Action: state.Plan.TargetedTest, Class: classify(err), Diagnostic: safeDiagnostic(err)})
				if err != nil {
					return state, err
				}
				break
			}
			state.Phase = PhaseFullVerify
		case PhaseDiagnoseRetry:
			if state.RetryCount >= c.maxRetries {
				state.Phase = PhaseFailed
				if err := c.persist(ctx, request.RunID, state); err != nil {
					return State{}, err
				}
				return state, ErrRetryExhausted
			}
			plan, err := c.model.Diagnose(ctx, request.Binding, state.LastFailure, append([]string(nil), state.FailedActions...))
			if err != nil {
				return c.fail(ctx, request, state, PhaseDiagnoseRetry, state.LastFailure.Action, "model", err)
			}
			if !validPlan(plan, request.Binding) || plan.Destructive || plan.Ambiguous || repeatedPlan(plan, state.FailedActions) {
				state.Phase = PhaseFailed
				if err := c.persist(ctx, request.RunID, state); err != nil {
					return State{}, err
				}
				return state, ErrRetryExhausted
			}
			state.Plan = plan
			state.RetryCount++
			state.Phase = PhaseParallel
		case PhaseFullVerify:
			if err := c.authority.FullVerify(ctx, request.Binding, state.Plan.FullVerify); err != nil {
				state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhaseFullVerify, Action: state.Plan.FullVerify, Class: classify(err), Diagnostic: safeDiagnostic(err)})
				if err != nil {
					return state, err
				}
				break
			}
			state.Phase = PhaseCheckpoint
		case PhaseCheckpoint:
			if err := c.authority.Checkpoint(ctx, request.Binding); err != nil {
				state, err = c.recordFailure(ctx, request, state, Failure{Phase: PhaseCheckpoint, Action: Action{Kind: "checkpoint"}, Class: classify(err), Diagnostic: safeDiagnostic(err)})
				if err != nil {
					return state, err
				}
				break
			}
			state.CheckpointID = "checkpoint_" + digestState(state)[:24]
			payload, _ := json.Marshal(state)
			if err := c.store.SaveCheckpoint(ctx, events.Checkpoint{ID: state.CheckpointID, RunID: request.RunID, CandidateSnapshot: request.Binding.CandidateSnapshot, EnvironmentID: request.Binding.EnvironmentID, PolicyVersion: request.Binding.PolicyVersion, State: string(PhaseCheckpoint), Payload: string(payload), CreatedAt: c.now()}); err != nil {
				return State{}, err
			}
			state.Phase = PhaseCompleted
			if err := c.store.UpdateRunStatus(ctx, request.RunID, "completed"); err != nil {
				return State{}, err
			}
		case PhaseCompleted, PhaseFailed, PhaseHumanCheckpoint:
			return state, terminalError(state)
		default:
			return State{}, ErrRejected
		}
		if err := c.persist(ctx, request.RunID, state); err != nil {
			return State{}, fmt.Errorf("loop state persistence: %w", err)
		}
	}
}

func (c *Controller) load(ctx context.Context, request Request) (State, error) {
	state := State{Phase: PhaseInspect, Binding: request.Binding}
	after := int64(0)
	for page := 0; page < 10; page++ {
		eventsPage, err := c.store.ListEvents(ctx, request.RunID, after, 1000)
		if err != nil {
			return State{}, err
		}
		for _, event := range eventsPage {
			after = event.Sequence
			if event.Type != "loop.state" {
				continue
			}
			var candidate State
			if json.Unmarshal([]byte(event.Payload), &candidate) != nil || candidate.Binding != request.Binding {
				return State{}, ErrRejected
			}
			state = candidate
		}
		if len(eventsPage) < 1000 {
			return state, nil
		}
	}
	return State{}, ErrRejected
}

func (c *Controller) persist(ctx context.Context, runID string, state State) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return ErrRejected
	}
	_, err = c.store.AppendEvent(ctx, runID, "loop.state", string(payload))
	return err
}

func (c *Controller) recordFailure(ctx context.Context, request Request, state State, failure Failure) (State, error) {
	fingerprint := actionFingerprint(failure.Action)
	if fingerprint == "" || contains(state.FailedActions, fingerprint) {
		state.Phase = PhaseFailed
		if err := c.persist(ctx, request.RunID, state); err != nil {
			return State{}, err
		}
		return state, ErrRetryExhausted
	}
	state.LastFailure = failure
	state.FailedActions = append(state.FailedActions, fingerprint)
	state.Phase = PhaseDiagnoseRetry
	return state, nil
}

func (c *Controller) fail(ctx context.Context, request Request, state State, phase Phase, action Action, class string, cause error) (State, error) {
	state.Phase = PhaseFailed
	state.LastFailure = Failure{Phase: phase, Action: action, Class: class, Diagnostic: safeDiagnostic(cause)}
	if err := c.persist(ctx, request.RunID, state); err != nil {
		return State{}, err
	}
	return state, cause
}

func validRequest(request Request) bool {
	return request.RunID != "" && validBinding(request.Binding)
}
func validBinding(binding Binding) bool {
	return binding.RepositoryID != "" && binding.CandidateSnapshot != "" && binding.EnvironmentID != "" && binding.PolicyVersion != ""
}
func validPlan(plan Plan, binding Binding) bool {
	if plan.Binding != binding || !validAction(plan.Patch) || !validAction(plan.TargetedTest) || !validAction(plan.FullVerify) || len(plan.Commands) == 0 || len(plan.Commands) > 64 {
		return false
	}
	for _, action := range plan.Commands {
		if !validAction(action) {
			return false
		}
	}
	return true
}
func validAction(action Action) bool {
	return action.ID != "" && len(action.ID) <= 128 && action.Kind != "" && len(action.Kind) <= 128 && action.Fingerprint != "" && len(action.Fingerprint) <= 128 && !strings.ContainsAny(action.ID+action.Kind+action.Fingerprint, "\x00\r\n")
}
func firstAction(actions []Action, kind string) Action {
	if len(actions) == 0 {
		return Action{Kind: kind}
	}
	return actions[0]
}
func repeatedPlan(plan Plan, failed []string) bool {
	for _, action := range append(append(append(append([]Action{}, plan.Commands...), plan.Patch), plan.TargetedTest), plan.FullVerify) {
		if contains(failed, actionFingerprint(action)) {
			return true
		}
	}
	return false
}
func actionFingerprint(action Action) string {
	if action.Fingerprint != "" {
		return action.Fingerprint
	}
	return ""
}
func digestState(state State) string {
	data, _ := json.Marshal(state)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func classify(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "execution"
}
func safeDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 1024 {
		value = value[:1024]
	}
	return value
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func terminalError(state State) error {
	switch state.Phase {
	case PhaseCompleted:
		return nil
	case PhaseHumanCheckpoint:
		return ErrHumanCheckpoint
	case PhaseFailed:
		return ErrRetryExhausted
	}
	return fmt.Errorf("loop not terminal: %s", state.Phase)
}
