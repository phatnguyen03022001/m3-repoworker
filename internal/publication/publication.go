// Package publication contains disabled-by-default, verified publication
// adapters. It never embeds credentials in commands and defaults to dry-run.
package publication

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	ErrRejected     = errors.New("publication request rejected")
	ErrUnauthorized = errors.New("publication authorization rejected")
	ErrDisabled     = errors.New("publication adapter disabled")
	ErrStale        = errors.New("verified candidate is stale")
)

type Kind string

const (
	KindGitCheckpoint Kind = "git_checkpoint"
	KindJJCheckpoint  Kind = "jj_checkpoint"
	KindGitPush       Kind = "git_push"
	KindGitHubPR      Kind = "github_pr"
	KindRelease       Kind = "release"
	KindDagger        Kind = "dagger"
	KindDagu          Kind = "dagu"
)

type Candidate struct {
	RepositoryRoot    string
	CandidateSnapshot string
	VerifiedSnapshot  string
	EnvironmentID     string
	PolicyVersion     string
	Verified          bool
}

type Request struct {
	Kind              Kind
	DryRun            bool
	ConfirmationToken string
	Remote            string
	Ref               string
	ExpectedRemoteRef string
	Base              string
	Head              string
	Title             string
	Body              string
	Workflow          string
}

type Gate struct {
	Enabled               bool
	AllowLocalMutation    bool
	AllowExternalMutation bool
	ConfirmationToken     string
}

type Command struct {
	Executable string   `json:"executable"`
	Arguments  []string `json:"arguments"`
}

type Result struct {
	Kind         Kind      `json:"kind"`
	DryRun       bool      `json:"dry_run"`
	Commands     []Command `json:"commands"`
	RemoteBefore string    `json:"remote_before,omitempty"`
	RemoteAfter  string    `json:"remote_after,omitempty"`
}

type Runner interface {
	Run(context.Context, string, string, ...string) (string, error)
}

type SnapshotProvider func(context.Context, string) (string, error)

type Adapter struct {
	gate     Gate
	runner   Runner
	snapshot SnapshotProvider
}

func New(gate Gate, runner Runner, snapshot SnapshotProvider) (*Adapter, error) {
	if runner == nil || snapshot == nil || len(gate.ConfirmationToken) > 256 || strings.ContainsAny(gate.ConfirmationToken, "\x00\r\n") {
		return nil, ErrRejected
	}
	return &Adapter{gate: gate, runner: runner, snapshot: snapshot}, nil
}

func (a *Adapter) Publish(ctx context.Context, candidate Candidate, request Request) (Result, error) {
	if ctx == nil || a == nil || !validCandidate(candidate) || !validRequest(request) || request.DryRun && request.ConfirmationToken != "" {
		return Result{}, ErrRejected
	}
	if current, err := a.snapshot(ctx, candidate.RepositoryRoot); err != nil || current != candidate.CandidateSnapshot || candidate.VerifiedSnapshot != candidate.CandidateSnapshot {
		return Result{}, ErrStale
	}
	commands, err := a.commands(candidate, request)
	if err != nil {
		return Result{}, err
	}
	result := Result{Kind: request.Kind, DryRun: request.DryRun, Commands: commands}
	if request.Kind == KindGitPush {
		before, err := a.remoteRef(ctx, candidate.RepositoryRoot, request)
		if err != nil {
			return Result{}, err
		}
		result.RemoteBefore = before
		if before != request.ExpectedRemoteRef {
			return Result{}, ErrStale
		}
	}
	if request.DryRun {
		return result, nil
	}
	if !a.gate.Enabled {
		return Result{}, ErrDisabled
	}
	if requiresExternal(request.Kind) {
		if !a.gate.AllowExternalMutation || request.ConfirmationToken == "" || request.ConfirmationToken != a.gate.ConfirmationToken {
			return Result{}, ErrUnauthorized
		}
	} else if !a.gate.AllowLocalMutation {
		return Result{}, ErrUnauthorized
	}
	for _, command := range commands {
		if _, err := a.runner.Run(ctx, candidate.RepositoryRoot, command.Executable, command.Arguments...); err != nil {
			return Result{}, ErrRejected
		}
	}
	if request.Kind == KindGitPush {
		after, err := a.remoteRef(ctx, candidate.RepositoryRoot, request)
		if err != nil {
			return Result{}, err
		}
		result.RemoteAfter = after
	}
	if current, err := a.snapshot(ctx, candidate.RepositoryRoot); err != nil || current != candidate.CandidateSnapshot {
		return Result{}, ErrStale
	}
	return result, nil
}

func (a *Adapter) commands(candidate Candidate, request Request) ([]Command, error) {
	message := request.Title
	if message == "" {
		message = "RepoWorker local checkpoint"
	}
	if !validText(message, 256) || !validText(request.Body, 16*1024) {
		return nil, ErrRejected
	}
	switch request.Kind {
	case KindGitCheckpoint:
		return []Command{{Executable: "git", Arguments: []string{"add", "-A", "--", "."}}, {Executable: "git", Arguments: []string{"commit", "--no-verify", "-m", message}}}, nil
	case KindJJCheckpoint:
		return []Command{{Executable: "jj", Arguments: []string{"commit", "-m", message}}}, nil
	case KindGitPush:
		if !validRef(request.Ref) || !validRemote(request.Remote) || !validExpectedRef(request.ExpectedRemoteRef) {
			return nil, ErrRejected
		}
		return []Command{{Executable: "git", Arguments: []string{"push", "--no-verify", request.Remote, "HEAD:refs/heads/" + request.Ref}}}, nil
	case KindGitHubPR:
		if !validRef(request.Base) || !validRef(request.Head) || !validText(request.Title, 256) || request.Body == "" {
			return nil, ErrRejected
		}
		return []Command{{Executable: "gh", Arguments: []string{"pr", "create", "--base", request.Base, "--head", request.Head, "--title", request.Title, "--body", request.Body}}}, nil
	case KindRelease:
		if !validRef(request.Ref) || !validText(request.Title, 256) {
			return nil, ErrRejected
		}
		return []Command{{Executable: "gh", Arguments: []string{"release", "create", request.Ref, "--title", request.Title, "--notes", request.Body, "--draft"}}}, nil
	case KindDagger:
		if !validRef(request.Ref) {
			return nil, ErrRejected
		}
		return []Command{{Executable: "dagger", Arguments: []string{"call", "publish", "--ref", request.Ref}}}, nil
	case KindDagu:
		if !validRef(request.Workflow) {
			return nil, ErrRejected
		}
		return []Command{{Executable: "dagu", Arguments: []string{"start", request.Workflow}}}, nil
	default:
		return nil, ErrRejected
	}
}

func (a *Adapter) remoteRef(ctx context.Context, root string, request Request) (string, error) {
	output, err := a.runner.Run(ctx, root, "git", "ls-remote", request.Remote, "refs/heads/"+request.Ref)
	if err != nil {
		return "", ErrRejected
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || len(fields[0]) != 40 || !validHex(fields[0]) || fields[1] != "refs/heads/"+request.Ref {
		return "", ErrRejected
	}
	return fields[0], nil
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, dir, executable string, args ...string) (string, error) {
	if ctx == nil || dir == "" || !filepath.IsAbs(dir) || !validExecutable(executable) {
		return "", ErrRejected
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return "", ErrRejected
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if len(output) > 64*1024 {
		output = output[:64*1024]
	}
	if err != nil {
		return string(output), ErrRejected
	}
	return string(output), nil
}

func requiresExternal(kind Kind) bool { return kind != KindGitCheckpoint && kind != KindJJCheckpoint }
func validCandidate(candidate Candidate) bool {
	return candidate.Verified && filepath.IsAbs(candidate.RepositoryRoot) && filepath.Clean(candidate.RepositoryRoot) == candidate.RepositoryRoot && validIdentity(candidate.CandidateSnapshot) && candidate.CandidateSnapshot == candidate.VerifiedSnapshot && candidate.EnvironmentID != "" && candidate.PolicyVersion != ""
}
func validRequest(request Request) bool {
	return request.Kind != "" && len(request.ConfirmationToken) <= 256 && !strings.ContainsAny(request.ConfirmationToken, "\x00\r\n")
}
func validIdentity(value string) bool { return len(value) == 64 && validHex(value) }
func validHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
func validRef(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n ~^:?*[\\") && !strings.Contains(value, "..")
}
func validExpectedRef(value string) bool { return value == "" || (len(value) == 40 && validHex(value)) }
func validRemote(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}
func validText(value string, max int) bool {
	return len(value) <= max && !strings.ContainsAny(value, "\x00\r\n") && !containsSecret(value)
}
func validExecutable(value string) bool {
	return value == "git" || value == "gh" || value == "dagger" || value == "dagu" || value == "jj"
}
func containsSecret(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization", "bearer ", "-----begin private key-----", "ghp_", "github_pat_", "sk-proj-", "access_token", "private_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
