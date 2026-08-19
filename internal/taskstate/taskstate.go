// Package taskstate provides fail-closed persistent development task handoff.
package taskstate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	stateVersion             = 1
	maxStateBytes      int64 = 64 << 10
	maxNextActionBytes       = 4 << 10
	maxBranchBytes           = 256
	gitTimeout               = 3 * time.Second
)

var (
	ErrRejected = errors.New("task state request rejected")
	taskIDRE    = regexp.MustCompile(`^task_[0-9a-f]{32}$`)
	shaRE       = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	identityRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// RepositoryState is the trusted local Git metadata bound to a task.
type RepositoryState struct {
	Branch string
	Head   string
}

// Inspector returns repository metadata without allowing arbitrary commands.
type Inspector interface {
	Snapshot(context.Context, string) (RepositoryState, error)
}

// State is the persisted handoff record returned through MCP.
type State struct {
	Version           int      `json:"version"`
	TaskID            string   `json:"task_id"`
	RepoRootIdentity  string   `json:"repo_root_identity"`
	Branch            string   `json:"branch"`
	BaseSHA           string   `json:"base_sha"`
	CurrentHeadSHA    string   `json:"current_head_sha"`
	LastVerifiedSHA   string   `json:"last_verified_sha"`
	VerificationState string   `json:"verification_state"`
	FailedChecks      []string `json:"failed_checks"`
	NextAction        string   `json:"next_action"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

// Store persists task state outside the repository and binds it to one
// canonical repository root.
type Store struct {
	repoRoot     string
	stateRoot    string
	repoIdentity string
	inspector    Inspector
	mu           sync.Mutex
}

// DefaultStateDir returns the per-user persistent state directory.
func DefaultStateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return "", ErrRejected
	}
	return filepath.Join(base, "m3-repoworker", "tasks"), nil
}

// New constructs a task store using a resolved, fixed Git executable and
// read-only Git metadata inspection.
func New(repoRoot, stateRoot string) (*Store, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, ErrRejected
	}
	if !filepath.IsAbs(gitPath) {
		gitPath, err = filepath.Abs(gitPath)
		if err != nil {
			return nil, ErrRejected
		}
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil || !filepath.IsAbs(gitPath) {
		return nil, ErrRejected
	}
	return NewWithInspector(repoRoot, stateRoot, gitInspector{executable: gitPath})
}

// NewWithInspector exists so tests can exercise persistence without executing
// Git. The production caller uses New.
func NewWithInspector(repoRoot, stateRoot string, inspector Inspector) (*Store, error) {
	if inspector == nil {
		return nil, ErrRejected
	}
	canonicalRepo, err := canonicalDirectory(repoRoot, false)
	if err != nil {
		return nil, ErrRejected
	}
	if stateRoot == "" || !filepath.IsAbs(stateRoot) || pathWithin(canonicalRepo, filepath.Clean(stateRoot)) {
		return nil, ErrRejected
	}
	canonicalState, err := canonicalDirectory(stateRoot, true)
	if err != nil || pathWithin(canonicalRepo, canonicalState) {
		return nil, ErrRejected
	}

	digest := sha256.Sum256([]byte(canonicalRepo))
	return &Store{
		repoRoot:     canonicalRepo,
		stateRoot:    canonicalState,
		repoIdentity: hex.EncodeToString(digest[:]),
		inspector:    inspector,
	}, nil
}

// Create starts a new task bound to the current branch and HEAD. Verification
// begins RED by construction.
func (s *Store) Create(ctx context.Context, nextAction string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !validNextAction(nextAction) {
		return State{}, ErrRejected
	}
	repoState, err := s.inspector.Snapshot(ctx, s.repoRoot)
	if err != nil || !validRepositoryState(repoState) {
		return State{}, ErrRejected
	}

	for attempts := 0; attempts < 4; attempts++ {
		taskID, err := newTaskID()
		if err != nil {
			return State{}, ErrRejected
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		state := State{
			Version:           stateVersion,
			TaskID:            taskID,
			RepoRootIdentity:  s.repoIdentity,
			Branch:            repoState.Branch,
			BaseSHA:           repoState.Head,
			CurrentHeadSHA:    repoState.Head,
			LastVerifiedSHA:   "",
			VerificationState: "RED",
			FailedChecks:      []string{},
			NextAction:        nextAction,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := validateState(state); err != nil {
			return State{}, ErrRejected
		}
		if _, err := os.Lstat(s.taskPath(taskID)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return State{}, ErrRejected
		}
		if err := s.save(state); err != nil {
			return State{}, ErrRejected
		}
		return state, nil
	}
	return State{}, ErrRejected
}

// Status returns persisted state without consulting or mutating the working
// repository.
func (s *Store) Status(_ context.Context, taskID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(taskID)
}

// Resume verifies repository identity and branch, refreshes HEAD, and marks the
// task RED when HEAD moved since the prior handoff.
func (s *Store) Resume(ctx context.Context, taskID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.load(taskID)
	if err != nil {
		return State{}, ErrRejected
	}
	repoState, err := s.inspector.Snapshot(ctx, s.repoRoot)
	if err != nil || !validRepositoryState(repoState) || repoState.Branch != state.Branch {
		return State{}, ErrRejected
	}
	if repoState.Head == state.CurrentHeadSHA {
		return state, nil
	}

	state.CurrentHeadSHA = repoState.Head
	state.VerificationState = "RED"
	state.FailedChecks = []string{}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.save(state); err != nil {
		return State{}, ErrRejected
	}
	return state, nil
}

func (s *Store) load(taskID string) (State, error) {
	if !taskIDRE.MatchString(taskID) {
		return State{}, ErrRejected
	}
	file, err := os.Open(s.taskPath(taskID))
	if err != nil {
		return State{}, ErrRejected
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStateBytes {
		return State{}, ErrRejected
	}

	decoder := json.NewDecoder(io.LimitReader(file, maxStateBytes+1))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, ErrRejected
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return State{}, ErrRejected
	}
	if err := validateState(state); err != nil || state.TaskID != taskID || state.RepoRootIdentity != s.repoIdentity {
		return State{}, ErrRejected
	}
	return state, nil
}

func (s *Store) save(state State) error {
	if err := validateState(state); err != nil || state.RepoRootIdentity != s.repoIdentity {
		return ErrRejected
	}
	repoStateDir := filepath.Join(s.stateRoot, s.repoIdentity)
	if err := os.MkdirAll(repoStateDir, 0o700); err != nil {
		return ErrRejected
	}
	if err := os.Chmod(repoStateDir, 0o700); err != nil {
		return ErrRejected
	}
	canonical, err := filepath.EvalSymlinks(repoStateDir)
	if err != nil || filepath.Clean(canonical) != filepath.Clean(repoStateDir) {
		return ErrRejected
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil || int64(len(data)+1) > maxStateBytes {
		return ErrRejected
	}
	data = append(data, '\n')

	temporary, err := os.CreateTemp(repoStateDir, ".task-*")
	if err != nil {
		return ErrRejected
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrRejected
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return ErrRejected
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrRejected
	}
	if err := temporary.Close(); err != nil {
		return ErrRejected
	}
	if err := os.Rename(temporaryPath, s.taskPath(state.TaskID)); err != nil {
		return ErrRejected
	}
	directory, err := os.Open(repoStateDir)
	if err != nil {
		return ErrRejected
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrRejected
	}
	return nil
}

func (s *Store) taskPath(taskID string) string {
	return filepath.Join(s.stateRoot, s.repoIdentity, taskID+".json")
}

func validateState(state State) error {
	if state.Version != stateVersion || !taskIDRE.MatchString(state.TaskID) || !identityRE.MatchString(state.RepoRootIdentity) {
		return ErrRejected
	}
	if !validBranch(state.Branch) || !shaRE.MatchString(state.BaseSHA) || !shaRE.MatchString(state.CurrentHeadSHA) {
		return ErrRejected
	}
	if state.LastVerifiedSHA != "" && !shaRE.MatchString(state.LastVerifiedSHA) {
		return ErrRejected
	}
	if state.VerificationState != "RED" && state.VerificationState != "GREEN" {
		return ErrRejected
	}
	if len(state.FailedChecks) > 128 || !validNextAction(state.NextAction) {
		return ErrRejected
	}
	for _, check := range state.FailedChecks {
		if check == "" || len(check) > 256 || !utf8.ValidString(check) || strings.ContainsRune(check, 0) || containsLikelySecret(check) {
			return ErrRejected
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, state.CreatedAt); err != nil {
		return ErrRejected
	}
	if _, err := time.Parse(time.RFC3339Nano, state.UpdatedAt); err != nil {
		return ErrRejected
	}
	return nil
}

func validRepositoryState(state RepositoryState) bool {
	return validBranch(state.Branch) && shaRE.MatchString(state.Head)
}

func validBranch(branch string) bool {
	return branch != "" && len(branch) <= maxBranchBytes && utf8.ValidString(branch) && !strings.ContainsAny(branch, "\x00\r\n")
}

func validNextAction(value string) bool {
	return len(value) <= maxNextActionBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && !containsLikelySecret(value)
}

func containsLikelySecret(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"-----begin private key-----",
		"-----begin rsa private key-----",
		"-----begin openssh private key-----",
		"authorization: bearer ",
		"github_pat_",
		"ghp_",
		"sk-proj-",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func newTaskID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(buffer), nil
}

func canonicalDirectory(path string, create bool) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", ErrRejected
	}
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", ErrRejected
		}
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrRejected
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", ErrRejected
	}
	return filepath.Clean(canonical), nil
}

func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}

type gitInspector struct {
	executable string
}

func (g gitInspector) Snapshot(ctx context.Context, repoRoot string) (RepositoryState, error) {
	if ctx == nil || g.executable == "" || !filepath.IsAbs(g.executable) {
		return RepositoryState{}, ErrRejected
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	top, err := runGit(ctx, g.executable, repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return RepositoryState{}, ErrRejected
	}
	canonicalTop, err := filepath.EvalSymlinks(top)
	if err != nil || filepath.Clean(canonicalTop) != filepath.Clean(repoRoot) {
		return RepositoryState{}, ErrRejected
	}
	branch, err := runGit(ctx, g.executable, repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || !validBranch(branch) {
		return RepositoryState{}, ErrRejected
	}
	head, err := runGit(ctx, g.executable, repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return RepositoryState{}, ErrRejected
	}
	head = strings.ToLower(head)
	if !shaRE.MatchString(head) {
		return RepositoryState{}, ErrRejected
	}
	return RepositoryState{Branch: branch, Head: head}, nil
}

func runGit(ctx context.Context, executable, repoRoot string, args ...string) (string, error) {
	fixed := []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", repoRoot}
	fixed = append(fixed, args...)
	command := exec.CommandContext(ctx, executable, fixed...)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git metadata unavailable")
	}
	value := strings.TrimSpace(string(output))
	if value == "" || strings.ContainsRune(value, 0) {
		return "", ErrRejected
	}
	return value, nil
}
