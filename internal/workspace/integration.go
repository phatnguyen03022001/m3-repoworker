package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	integrationPrepared    = "PREPARED"
	integrationApplying    = "APPLYING"
	integrationCommitted   = "COMMITTED"
	integrationQuarantined = "QUARANTINED"
	maxIntegrationBlob     = 4 << 20
)

type IntegrationStep struct {
	Path         string `json:"path"`
	Operation    string `json:"operation"`
	BeforeDigest string `json:"before_digest"`
	AfterDigest  string `json:"after_digest"`
	Mode         uint32 `json:"mode"`
	StagedBlob   []byte `json:"staged_blob,omitempty"`
}

type IntegrationPlan struct {
	RepositoryID      string            `json:"repository_id"`
	SourceFilesystem  string            `json:"source_filesystem"`
	GenerationID      string            `json:"generation_id"`
	LeaseGeneration   uint64            `json:"lease_generation"`
	BaseSnapshot      string            `json:"base_snapshot"`
	CandidateSnapshot string            `json:"candidate_snapshot"`
	Steps             []IntegrationStep `json:"steps"`
	PlanDigest        string            `json:"plan_digest"`
}

type IntegrationJournal struct {
	ID        string          `json:"id"`
	State     string          `json:"state"`
	Cursor    int             `json:"cursor"`
	Plan      IntegrationPlan `json:"plan"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type fileState struct {
	Exists bool
	Digest string
	Mode   uint32
}

// BuildIntegrationPlan computes a deterministic, candidate-bound file plan.
// A plan is displayable data; only ApplyIntegration can mutate the live root.
func (r *Repository) BuildIntegrationPlan(ctx context.Context, generation Generation, lease Lease) (IntegrationPlan, error) {
	if err := r.AssertLease(ctx, lease); err != nil {
		return IntegrationPlan{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r.rootFD == nil {
		return IntegrationPlan{}, ErrRejected
	}
	loaded, err := r.loadGeneration(generation.ID)
	if err != nil || loaded.CandidateSnapshot != generation.CandidateSnapshot || lease.FencingGeneration == 0 {
		return IntegrationPlan{}, ErrRejected
	}
	baseSnapshot, err := snapshotTree(ctx, r.root)
	if err != nil {
		return IntegrationPlan{}, ErrRejected
	}
	candidateSnapshot, err := snapshotTree(ctx, generation.Path)
	if err != nil || candidateSnapshot == "" {
		return IntegrationPlan{}, ErrRejected
	}
	baseFiles, err := treeFiles(ctx, r.root)
	if err != nil {
		return IntegrationPlan{}, ErrRejected
	}
	candidateFiles, err := treeFiles(ctx, generation.Path)
	if err != nil {
		return IntegrationPlan{}, ErrRejected
	}
	paths := make([]string, 0, len(baseFiles)+len(candidateFiles))
	seen := map[string]struct{}{}
	for path := range baseFiles {
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range candidateFiles {
		if _, ok := seen[path]; !ok {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	plan := IntegrationPlan{
		RepositoryID: r.rootIdentity, SourceFilesystem: r.filesystemID,
		GenerationID: generation.ID, LeaseGeneration: lease.FencingGeneration,
		BaseSnapshot: baseSnapshot, CandidateSnapshot: candidateSnapshot,
	}
	for _, path := range paths {
		before := baseFiles[path]
		after := candidateFiles[path]
		if before.Digest == after.Digest && before.Exists == after.Exists {
			continue
		}
		step := IntegrationStep{Path: path, BeforeDigest: before.Digest, AfterDigest: after.Digest, Mode: after.Mode}
		switch {
		case !before.Exists && after.Exists:
			step.Operation = "create"
		case before.Exists && !after.Exists:
			step.Operation = "delete"
		case before.Exists && after.Exists:
			step.Operation = "update"
		default:
			return IntegrationPlan{}, ErrRejected
		}
		if after.Exists {
			data, err := os.ReadFile(filepath.Join(generation.Path, filepath.FromSlash(path)))
			if err != nil || len(data) > maxIntegrationBlob {
				return IntegrationPlan{}, ErrRejected
			}
			step.StagedBlob = data
		}
		plan.Steps = append(plan.Steps, step)
	}
	plan.PlanDigest, err = computePlanDigest(plan)
	if err != nil {
		return IntegrationPlan{}, ErrRejected
	}
	return plan, nil
}

// ApplyIntegration advances a durable per-step journal. It never claims
// filesystem atomicity: a restart observes each step and either advances,
// retries with a live fence, or quarantines ambiguity.
func (r *Repository) ApplyIntegration(ctx context.Context, plan IntegrationPlan, lease Lease) (IntegrationJournal, error) {
	if err := r.AssertLease(ctx, lease); err != nil {
		return IntegrationJournal{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r.rootFD == nil || plan.RepositoryID != r.rootIdentity || plan.SourceFilesystem != r.filesystemID ||
		plan.GenerationID != lease.GenerationID || plan.LeaseGeneration != lease.FencingGeneration {
		return IntegrationJournal{}, ErrRejected
	}
	digest, err := computePlanDigest(plan)
	if err != nil || digest != plan.PlanDigest || !validIntegrationPlan(plan) {
		return IntegrationJournal{}, ErrRejected
	}
	base, err := snapshotTree(ctx, r.root)
	if err != nil || base != plan.BaseSnapshot {
		return IntegrationJournal{}, ErrStaleFence
	}
	id, err := newJournalID()
	if err != nil {
		return IntegrationJournal{}, ErrRejected
	}
	journal := IntegrationJournal{ID: id, State: integrationPrepared, Plan: plan, UpdatedAt: time.Now().UTC()}
	path := filepath.Join(r.journalRoot, id+".json")
	if err := writeJSONAtomic(path, journal, 0o600); err != nil {
		return IntegrationJournal{}, ErrRejected
	}
	for journal.Cursor < len(plan.Steps) {
		if err := r.applyIntegrationStep(ctx, plan.Steps[journal.Cursor]); err != nil {
			journal.State = integrationQuarantined
			journal.UpdatedAt = time.Now().UTC()
			_ = writeJSONAtomic(path, journal, 0o600)
			return IntegrationJournal{}, err
		}
		journal.State = integrationApplying
		journal.Cursor++
		journal.UpdatedAt = time.Now().UTC()
		if err := writeJSONAtomic(path, journal, 0o600); err != nil {
			return IntegrationJournal{}, ErrRejected
		}
	}
	journal.State = integrationCommitted
	journal.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(path, journal, 0o600); err != nil {
		return IntegrationJournal{}, ErrRejected
	}
	return journal, nil
}

func (r *Repository) applyIntegrationStep(ctx context.Context, step IntegrationStep) error {
	if ctx == nil || !validRelativePath(step.Path) || (step.Operation != "create" && step.Operation != "update" && step.Operation != "delete") {
		return ErrRejected
	}
	current, err := fileStateAt(r.root, step.Path)
	if err != nil || current.Digest != step.BeforeDigest {
		return ErrStaleFence
	}
	if step.Operation == "delete" {
		parent, name, err := r.openParentDirectory(step.Path, false)
		if err != nil {
			return ErrRejected
		}
		defer unix.Close(parent)
		if err := unix.Unlinkat(parent, name, 0); err != nil {
			return ErrRejected
		}
		if err := syncFD(parent); err != nil {
			return ErrRejected
		}
	} else {
		if len(step.StagedBlob) > maxIntegrationBlob {
			return ErrRejected
		}
		parent, name, err := r.openParentDirectory(step.Path, true)
		if err != nil {
			return ErrRejected
		}
		defer unix.Close(parent)
		temporaryName := ".repoworker-tmp-" + hex.EncodeToString(randomBytes(8))
		fd, err := unix.Openat(parent, temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, step.Mode&0o777)
		if err != nil {
			return ErrRejected
		}
		file := os.NewFile(uintptr(fd), temporaryName)
		if file == nil {
			_ = unix.Close(fd)
			return ErrRejected
		}
		if err := unix.Fchmod(fd, step.Mode&0o777); err != nil {
			_ = file.Close()
			return ErrRejected
		}
		if _, err := file.Write(step.StagedBlob); err != nil {
			_ = file.Close()
			return ErrRejected
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return ErrRejected
		}
		if err := file.Close(); err != nil || unix.Renameat(parent, temporaryName, parent, name) != nil {
			_ = unix.Unlinkat(parent, temporaryName, 0)
			return ErrRejected
		}
		if err := syncFD(parent); err != nil {
			return ErrRejected
		}
	}
	after, err := fileStateAt(r.root, step.Path)
	if err != nil || after.Digest != step.AfterDigest {
		return ErrRejected
	}
	return nil
}

func (r *Repository) openParentDirectory(relative string, create bool) (int, string, error) {
	if !validRelativePath(relative) || r.rootFD == nil {
		return -1, "", ErrRejected
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	parentFD, err := unix.Dup(int(r.rootFD.Fd()))
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err := unix.Mkdirat(parentFD, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				_ = unix.Close(parentFD)
				return -1, "", err
			}
			next, err = unix.Openat(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			_ = unix.Close(parentFD)
			return -1, "", err
		}
		_ = unix.Close(parentFD)
		parentFD = next
	}
	return parentFD, parts[len(parts)-1], nil
}

func (r *Repository) recoverJournals(ctx context.Context) error {
	entries, err := os.ReadDir(r.journalRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var journal IntegrationJournal
		if err := readJSON(filepath.Join(r.journalRoot, entry.Name()), &journal); err != nil {
			return ErrRejected
		}
		if journal.State == integrationCommitted {
			continue
		}
		if err := r.recoverJournal(ctx, filepath.Join(r.journalRoot, entry.Name()), &journal); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) recoverJournal(ctx context.Context, path string, journal *IntegrationJournal) error {
	if journal.State != integrationPrepared && journal.State != integrationApplying {
		return ErrRejected
	}
	if !validIntegrationPlan(journal.Plan) || journal.Cursor < 0 || journal.Cursor > len(journal.Plan.Steps) {
		return ErrRejected
	}
	for journal.Cursor < len(journal.Plan.Steps) {
		step := journal.Plan.Steps[journal.Cursor]
		current, err := fileStateAt(r.root, step.Path)
		if err != nil {
			return ErrRejected
		}
		switch current.Digest {
		case step.AfterDigest:
			journal.Cursor++
			journal.State = integrationApplying
			journal.UpdatedAt = time.Now().UTC()
			if err := writeJSONAtomic(path, journal, 0o600); err != nil {
				return ErrRejected
			}
		case step.BeforeDigest:
			journal.State = integrationQuarantined
			journal.UpdatedAt = time.Now().UTC()
			_ = writeJSONAtomic(path, journal, 0o600)
			return ErrRejected
		default:
			journal.State = integrationQuarantined
			journal.UpdatedAt = time.Now().UTC()
			_ = writeJSONAtomic(path, journal, 0o600)
			return ErrRejected
		}
	}
	journal.State = integrationCommitted
	journal.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(path, journal, 0o600)
}

func treeFiles(ctx context.Context, root string) (map[string]fileState, error) {
	files := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if excluded(relative) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrRejected
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return ErrRejected
		}
		state, err := fileStateAt(root, filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = state
		return ctx.Err()
	})
	return files, err
}

func fileStateAt(root, relative string) (fileState, error) {
	if !validRelativePath(relative) {
		return fileState{}, ErrRejected
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fileState{}, ErrRejected
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileState{}, ErrRejected
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		_ = file.Close()
		return fileState{}, ErrRejected
	}
	if err := file.Close(); err != nil {
		return fileState{}, ErrRejected
	}
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte{byte(info.Mode().Perm() >> 8), byte(info.Mode().Perm())})
	return fileState{Exists: true, Digest: hex.EncodeToString(hash.Sum(nil)), Mode: uint32(info.Mode().Perm())}, nil
}

func computePlanDigest(plan IntegrationPlan) (string, error) {
	plan.PlanDigest = ""
	data, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validIntegrationPlan(plan IntegrationPlan) bool {
	if plan.RepositoryID == "" || plan.SourceFilesystem == "" || !validGenerationID(plan.GenerationID) ||
		plan.LeaseGeneration == 0 || plan.BaseSnapshot == "" || plan.CandidateSnapshot == "" || plan.PlanDigest == "" {
		return false
	}
	previous := ""
	for _, step := range plan.Steps {
		if !validRelativePath(step.Path) || step.Path <= previous || step.BeforeDigest == step.AfterDigest ||
			(step.Operation != "create" && step.Operation != "update" && step.Operation != "delete") || len(step.StagedBlob) > maxIntegrationBlob {
			return false
		}
		if step.Operation == "delete" && len(step.StagedBlob) != 0 {
			return false
		}
		previous = step.Path
	}
	return true
}

func validRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean != path || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, ".git/") {
		return false
	}
	return true
}

func syncFD(fd int) error {
	return unix.Fsync(fd)
}

func newJournalID() (string, error) {
	data := randomBytes(16)
	if len(data) == 0 {
		return "", ErrRejected
	}
	return "journal_" + hex.EncodeToString(data), nil
}

func randomBytes(size int) []byte {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return nil
	}
	return data
}
