// Package workspace owns isolated task-workspace generations and their live
// fences. A generation is always materialized outside the live repository.
package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	activeState      = "ACTIVE"
	quarantinedState = "QUARANTINED"
	maxGenerationID  = 64
)

var (
	ErrRejected   = errors.New("workspace request rejected")
	ErrLeaseBusy  = errors.New("workspace lease unavailable")
	ErrStaleFence = errors.New("workspace fence is stale")
)

// Repository is the authority for one live repository root and its isolated
// generation store. It never treats a generated workspace as the repository.
type Repository struct {
	root           string
	rootFD         *os.File
	stateLock      *os.File
	rootIdentity   string
	filesystemID   string
	workspaceRoot  string
	journalRoot    string
	mu             sync.Mutex
	nextGeneration uint64
}

type Generation struct {
	ID                string    `json:"id"`
	Path              string    `json:"path"`
	RepositoryID      string    `json:"repository_id"`
	SourceFilesystem  string    `json:"source_filesystem"`
	CandidateSnapshot string    `json:"candidate_snapshot"`
	FencingGeneration uint64    `json:"fencing_generation"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
}

type Lease struct {
	GenerationID      string    `json:"generation_id"`
	Owner             string    `json:"owner"`
	FencingGeneration uint64    `json:"fencing_generation"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type leaseFile struct {
	Lease Lease `json:"lease"`
}

type runtimeOwner struct {
	Owner             string    `json:"owner"`
	FencingGeneration uint64    `json:"fencing_generation"`
	ReservedAt        time.Time `json:"reserved_at"`
}

// OpenRepository validates the live root and creates only state under the
// caller-provided directory outside that root.
func OpenRepository(root, stateRoot string) (*Repository, error) {
	if root == "" || stateRoot == "" || !filepath.IsAbs(root) || !filepath.IsAbs(stateRoot) {
		return nil, ErrRejected
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, ErrRejected
	}
	canonicalRoot = filepath.Clean(canonicalRoot)
	canonicalState, err := canonicalDirectory(stateRoot)
	if err != nil || pathWithin(canonicalRoot, canonicalState) {
		return nil, ErrRejected
	}
	rootFD, err := unix.Open(canonicalRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrRejected
	}
	filesystemID, err := fileIdentity(rootFD)
	if err != nil {
		_ = unix.Close(rootFD)
		return nil, ErrRejected
	}
	rootIdentity := digestString(canonicalRoot)
	workspaceRoot := filepath.Join(canonicalState, rootIdentity, "workspaces")
	journalRoot := filepath.Join(canonicalState, rootIdentity, "journals")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil || os.Chmod(workspaceRoot, 0o700) != nil ||
		os.MkdirAll(journalRoot, 0o700) != nil || os.Chmod(journalRoot, 0o700) != nil {
		_ = unix.Close(rootFD)
		return nil, ErrRejected
	}
	lockPath := filepath.Join(canonicalState, rootIdentity+".lock")
	stateLock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|unix.O_CLOEXEC, 0o600)
	if err != nil || unix.Flock(int(stateLock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		if stateLock != nil {
			_ = stateLock.Close()
		}
		_ = unix.Close(rootFD)
		return nil, ErrRejected
	}
	repository := &Repository{
		root:          canonicalRoot,
		rootFD:        os.NewFile(uintptr(rootFD), canonicalRoot),
		stateLock:     stateLock,
		rootIdentity:  rootIdentity,
		filesystemID:  filesystemID,
		workspaceRoot: workspaceRoot,
		journalRoot:   journalRoot,
	}
	if repository.rootFD == nil || repository.recoverJournals(context.Background()) != nil {
		_ = unix.Close(rootFD)
		_ = unix.Flock(int(stateLock.Fd()), unix.LOCK_UN)
		_ = stateLock.Close()
		return nil, ErrRejected
	}
	return repository, nil
}

func (r *Repository) RootIdentity() string { return r.rootIdentity }

func (r *Repository) SourceFilesystemIdentity() string { return r.filesystemID }

func (r *Repository) LiveRoot() string { return r.root }

// LoadGeneration revalidates a persisted generation against the opened live
// repository authority. It is used by startup recovery and loop resume.
func (r *Repository) LoadGeneration(ctx context.Context, generationID string) (Generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r == nil || r.rootFD == nil {
		return Generation{}, ErrRejected
	}
	return r.loadGeneration(generationID)
}

// CurrentLease returns the persisted lease for recovery validation without
// granting ownership.
func (r *Repository) CurrentLease(ctx context.Context, generationID string) (Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r == nil || r.rootFD == nil {
		return Lease{}, ErrRejected
	}
	generation, err := r.loadGeneration(generationID)
	if err != nil {
		return Lease{}, err
	}
	var current leaseFile
	if err := readJSON(filepath.Join(generation.Path, ".lease.json"), &current); err != nil || current.Lease.GenerationID != generationID || !validOwner(current.Lease.Owner) || current.Lease.FencingGeneration == 0 || current.Lease.ExpiresAt.IsZero() {
		return Lease{}, ErrRejected
	}
	return current.Lease, nil
}

// Recover removes old process ownership after runtime recovery. It validates
// every generation before making a stale lease reusable; ambiguous records are
// moved to a private quarantine directory and cannot be mutated.
func (r *Repository) Recover(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r == nil || r.rootFD == nil {
		return ErrRejected
	}
	entries, err := os.ReadDir(r.workspaceRoot)
	if err != nil {
		return ErrRejected
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".tmp-") {
			if quarantineErr := r.quarantineGeneration(entry.Name()); quarantineErr != nil {
				return ErrRejected
			}
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "gen_") {
			continue
		}
		generation, err := r.loadGeneration(entry.Name())
		if err != nil {
			if quarantineErr := r.quarantineGeneration(entry.Name()); quarantineErr != nil {
				return ErrRejected
			}
			continue
		}
		leasePath := filepath.Join(generation.Path, ".lease.json")
		var lease leaseFile
		if err := readJSON(leasePath, &lease); err == nil {
			if lease.Lease.GenerationID != generation.ID || !validOwner(lease.Lease.Owner) || lease.Lease.FencingGeneration == 0 || lease.Lease.ExpiresAt.IsZero() {
				if quarantineErr := r.quarantineGeneration(generation.ID); quarantineErr != nil {
					return ErrRejected
				}
				continue
			}
			if err := os.Remove(leasePath); err != nil {
				return ErrRejected
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			if quarantineErr := r.quarantineGeneration(generation.ID); quarantineErr != nil {
				return ErrRejected
			}
			continue
		}
		if err := os.Remove(filepath.Join(generation.Path, ".runtime-owner.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrRejected
		}
		if err := syncDirectory(generation.Path); err != nil {
			return ErrRejected
		}
	}
	return syncDirectory(r.workspaceRoot)
}

func (r *Repository) quarantineGeneration(id string) error {
	path := filepath.Join(r.workspaceRoot, id)
	quarantineRoot := filepath.Join(r.workspaceRoot, "quarantine")
	if err := os.MkdirAll(quarantineRoot, 0o700); err != nil {
		return err
	}
	quarantinePath := filepath.Join(quarantineRoot, id+"-"+strconv.FormatInt(time.Now().UTC().UnixNano(), 10))
	if err := os.Rename(path, quarantinePath); err != nil {
		return err
	}
	return syncDirectory(r.workspaceRoot)
}

// SnapshotPath computes the same candidate snapshot used by generation
// materialization and refresh. It is read-only and uses the shared walker.
func SnapshotPath(ctx context.Context, root string) (string, error) {
	if ctx == nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrRejected
	}
	return snapshotTree(ctx, root)
}

// Close releases the authority descriptor for the live repository.
func (r *Repository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rootFD == nil {
		return nil
	}
	err := r.rootFD.Close()
	r.rootFD = nil
	if r.stateLock != nil {
		_ = unix.Flock(int(r.stateLock.Fd()), unix.LOCK_UN)
		_ = r.stateLock.Close()
		r.stateLock = nil
	}
	return err
}

// Materialize creates a new generation from the live root. Source identity
// and content are checked both before and after copying so a concurrent live
// mutation cannot silently become a candidate workspace.
func (r *Repository) Materialize(ctx context.Context) (Generation, error) {
	if ctx == nil || r == nil || r.root == "" {
		return Generation{}, ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.assertSource(); err != nil {
		return Generation{}, err
	}
	before, err := snapshotTree(ctx, r.root)
	if err != nil {
		return Generation{}, ErrRejected
	}
	id, err := newGenerationID()
	if err != nil {
		return Generation{}, ErrRejected
	}
	temporary := filepath.Join(r.workspaceRoot, ".tmp-"+id)
	finalPath := filepath.Join(r.workspaceRoot, id)
	if err := os.Mkdir(temporary, 0o700); err != nil {
		return Generation{}, ErrRejected
	}
	defer os.RemoveAll(temporary)
	if err := copyTree(ctx, r.root, temporary); err != nil {
		return Generation{}, ErrRejected
	}
	if err := r.assertSource(); err != nil {
		return Generation{}, ErrRejected
	}
	after, err := snapshotTree(ctx, r.root)
	if err != nil || before != after {
		return Generation{}, ErrRejected
	}
	if err := syncDirectory(temporary); err != nil {
		return Generation{}, ErrRejected
	}
	if err := os.Rename(temporary, finalPath); err != nil {
		return Generation{}, ErrRejected
	}
	generation := Generation{
		ID: id, Path: finalPath, RepositoryID: r.rootIdentity,
		SourceFilesystem: r.filesystemID, CandidateSnapshot: before,
		State: activeState, CreatedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(finalPath, ".generation.json"), generation, 0o600); err != nil {
		return Generation{}, ErrRejected
	}
	if err := syncDirectory(r.workspaceRoot); err != nil {
		return Generation{}, ErrRejected
	}
	return generation, nil
}

func (r *Repository) loadGeneration(id string) (Generation, error) {
	if !validGenerationID(id) {
		return Generation{}, ErrRejected
	}
	path := filepath.Join(r.workspaceRoot, id)
	var generation Generation
	if err := readJSON(filepath.Join(path, ".generation.json"), &generation); err != nil {
		return Generation{}, ErrRejected
	}
	if generation.ID != id || generation.Path != path || generation.RepositoryID != r.rootIdentity ||
		generation.SourceFilesystem != r.filesystemID || generation.State != activeState || generation.CandidateSnapshot == "" {
		return Generation{}, ErrRejected
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || pathWithin(r.root, path) {
		return Generation{}, ErrRejected
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(canonicalPath) != path {
		return Generation{}, ErrRejected
	}
	return generation, nil
}

// AcquireLease fences every new ownership epoch. Expired leases quarantine
// their generation instead of allowing an uncertain owner to resume mutation.
func (r *Repository) AcquireLease(ctx context.Context, generationID, owner string, ttl time.Duration) (Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || !validOwner(owner) || ttl <= 0 || ttl > 24*time.Hour {
		return Lease{}, ErrRejected
	}
	generation, err := r.loadGeneration(generationID)
	if err != nil {
		return Lease{}, err
	}
	leasePath := filepath.Join(generation.Path, ".lease.json")
	var existing leaseFile
	if err := readJSON(leasePath, &existing); err == nil {
		if existing.Lease.ExpiresAt.After(time.Now().UTC()) {
			return Lease{}, ErrLeaseBusy
		}
		generation.State = quarantinedState
		if err := writeJSONAtomic(filepath.Join(generation.Path, ".generation.json"), generation, 0o600); err != nil {
			return Lease{}, ErrRejected
		}
		return Lease{}, ErrStaleFence
	} else if !errors.Is(err, os.ErrNotExist) {
		return Lease{}, ErrRejected
	}
	fence, err := r.nextFence()
	if err != nil {
		return Lease{}, ErrRejected
	}
	lease := Lease{GenerationID: generationID, Owner: owner, FencingGeneration: fence, ExpiresAt: time.Now().UTC().Add(ttl)}
	if err := writeJSONAtomic(leasePath, leaseFile{Lease: lease}, 0o600); err != nil {
		return Lease{}, ErrRejected
	}
	return lease, nil
}

func (r *Repository) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || !validOwner(lease.Owner) || !validGenerationID(lease.GenerationID) || ttl <= 0 || ttl > 24*time.Hour {
		return Lease{}, ErrRejected
	}
	generation, err := r.loadGeneration(lease.GenerationID)
	if err != nil {
		return Lease{}, err
	}
	path := filepath.Join(generation.Path, ".lease.json")
	var current leaseFile
	if err := readJSON(path, &current); err != nil || current.Lease != lease || !current.Lease.ExpiresAt.After(time.Now().UTC()) {
		return Lease{}, ErrStaleFence
	}
	lease.ExpiresAt = time.Now().UTC().Add(ttl)
	if err := writeJSONAtomic(path, leaseFile{Lease: lease}, 0o600); err != nil {
		return Lease{}, ErrRejected
	}
	return lease, nil
}

func (r *Repository) ReleaseLease(ctx context.Context, lease Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || !validOwner(lease.Owner) || !validGenerationID(lease.GenerationID) {
		return ErrRejected
	}
	generation, err := r.loadGeneration(lease.GenerationID)
	if err != nil {
		return err
	}
	path := filepath.Join(generation.Path, ".lease.json")
	var current leaseFile
	if err := readJSON(path, &current); err != nil || current.Lease != lease {
		return ErrStaleFence
	}
	if err := os.Remove(path); err != nil {
		return ErrRejected
	}
	return syncDirectory(generation.Path)
}

// DiscardGeneration permanently removes an isolated candidate only after its
// lease has been released. It never touches the live repository.
func (r *Repository) DiscardGeneration(ctx context.Context, generationID string) error {
	if ctx == nil || r == nil || !validGenerationID(generationID) {
		return ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rootFD == nil {
		return ErrRejected
	}
	path := filepath.Join(r.workspaceRoot, generationID)
	if pathWithin(r.root, path) {
		return ErrRejected
	}
	if _, err := os.Stat(filepath.Join(path, ".lease.json")); err == nil {
		return ErrLeaseBusy
	}
	if err := os.RemoveAll(path); err != nil {
		return ErrRejected
	}
	return syncDirectory(r.workspaceRoot)
}

// RefreshGeneration records the new candidate snapshot after a mutation that
// was performed inside the generation. It never touches the live repository;
// the active lease remains the mutation fence for the metadata update.
func (r *Repository) RefreshGeneration(ctx context.Context, generation Generation, lease Lease) (Generation, error) {
	if err := r.AssertGeneration(ctx, generation, lease); err != nil {
		return Generation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || r.rootFD == nil {
		return Generation{}, ErrRejected
	}
	loaded, err := r.loadGeneration(generation.ID)
	if err != nil || loaded.Path != generation.Path {
		return Generation{}, ErrStaleFence
	}
	snapshot, err := snapshotTree(ctx, loaded.Path)
	if err != nil || snapshot == "" {
		return Generation{}, ErrRejected
	}
	loaded.CandidateSnapshot = snapshot
	if err := writeJSONAtomic(filepath.Join(loaded.Path, ".generation.json"), loaded, 0o600); err != nil {
		return Generation{}, ErrRejected
	}
	return loaded, nil
}

// AssertLease is the mutation fence used by integration and runtime layers.
func (r *Repository) AssertLease(ctx context.Context, lease Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil || !validGenerationID(lease.GenerationID) || !validOwner(lease.Owner) {
		return ErrRejected
	}
	generation, err := r.loadGeneration(lease.GenerationID)
	if err != nil {
		return err
	}
	var current leaseFile
	if err := readJSON(filepath.Join(generation.Path, ".lease.json"), &current); err != nil || current.Lease != lease {
		return ErrStaleFence
	}
	if !lease.ExpiresAt.After(time.Now().UTC()) {
		return ErrStaleFence
	}
	return nil
}

// AssertGeneration binds a runtime or integration owner to the exact
// generation metadata and live lease fence.
func (r *Repository) AssertGeneration(ctx context.Context, generation Generation, lease Lease) error {
	if err := r.AssertLease(ctx, lease); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	loaded, err := r.loadGeneration(generation.ID)
	if err != nil || loaded.Path != generation.Path || loaded.CandidateSnapshot != generation.CandidateSnapshot || lease.GenerationID != generation.ID {
		return ErrStaleFence
	}
	return nil
}

// ReserveRuntime permits one runtime owner for a generation and binds the
// reservation to the active lease fence.
func (r *Repository) ReserveRuntime(ctx context.Context, lease Lease, owner string) error {
	if err := r.AssertLease(ctx, lease); err != nil {
		return err
	}
	if !validOwner(owner) {
		return ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	generation, err := r.loadGeneration(lease.GenerationID)
	if err != nil {
		return err
	}
	path := filepath.Join(generation.Path, ".runtime-owner.json")
	var existing runtimeOwner
	if err := readJSON(path, &existing); err == nil {
		return ErrLeaseBusy
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrRejected
	}
	return writeJSONAtomic(path, runtimeOwner{Owner: owner, FencingGeneration: lease.FencingGeneration, ReservedAt: time.Now().UTC()}, 0o600)
}

func (r *Repository) ReleaseRuntime(ctx context.Context, lease Lease, owner string) error {
	if err := r.AssertLease(ctx, lease); err != nil {
		return err
	}
	if !validOwner(owner) {
		return ErrRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	generation, err := r.loadGeneration(lease.GenerationID)
	if err != nil {
		return err
	}
	path := filepath.Join(generation.Path, ".runtime-owner.json")
	var current runtimeOwner
	if err := readJSON(path, &current); err != nil || current.Owner != owner || current.FencingGeneration != lease.FencingGeneration {
		return ErrStaleFence
	}
	if err := os.Remove(path); err != nil {
		return ErrRejected
	}
	return syncDirectory(generation.Path)
}

func (r *Repository) nextFence() (uint64, error) {
	path := filepath.Join(r.workspaceRoot, ".fence")
	var current uint64
	if data, err := os.ReadFile(path); err == nil {
		value, parseErr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if parseErr != nil {
			return 0, ErrRejected
		}
		current = value
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, ErrRejected
	}
	if current == ^uint64(0) {
		return 0, ErrRejected
	}
	current++
	if err := writeBytesAtomic(path, []byte(strconv.FormatUint(current, 10)+"\n"), 0o600); err != nil {
		return 0, ErrRejected
	}
	return current, nil
}

func (r *Repository) assertSource() error {
	if r.rootFD == nil {
		return ErrRejected
	}
	identity, err := fileIdentity(int(r.rootFD.Fd()))
	if err != nil || identity != r.filesystemID {
		return ErrRejected
	}
	return nil
}

func snapshotTree(ctx context.Context, root string) (string, error) {
	hash := sha256.New()
	var paths []string
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
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, relative := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(root, relative)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
			return "", ErrRejected
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\x00", filepath.ToSlash(relative), info.Mode().Perm(), info.Size())
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(hash, file); err != nil {
				_ = file.Close()
				return "", err
			}
			if err := file.Close(); err != nil {
				return "", err
			}
		}
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyTree(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if excluded(relative) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrRejected
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Mkdir(target, info.Mode().Perm())
		}
		if !entry.Type().IsRegular() {
			return ErrRejected
		}
		if err := unix.Clonefile(path, target, 0); err != nil {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if err := copyRegular(path, target, info.Mode().Perm()); err != nil {
				return err
			}
		}
		return nil
	})
}

func copyRegular(source, destination string, mode fs.FileMode) error {
	input, err := os.OpenFile(source, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func excluded(relative string) bool {
	first := strings.Split(filepath.ToSlash(relative), "/")[0]
	return first == ".git" || first == ".cache" || first == "bin" || first == ".repoworker-state" ||
		(first == ".generation.json" || first == ".lease.json" || first == ".runtime-owner.json")
}

func fileIdentity(fd int) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	return digestString(fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))), nil
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func canonicalDirectory(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", ErrRejected
	}
	return filepath.Clean(canonical), nil
}

func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}

func newGenerationID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "gen_" + hex.EncodeToString(buffer), nil
}

func validGenerationID(id string) bool {
	return len(id) <= maxGenerationID && strings.HasPrefix(id, "gen_") && !strings.ContainsAny(id, "/\\\x00")
}

func validOwner(owner string) bool {
	return owner != "" && len(owner) <= 256 && !strings.ContainsAny(owner, "/\\\x00\r\n")
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrRejected
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrRejected
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeBytesAtomic(path, append(data, '\n'), mode)
}

func writeBytesAtomic(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
