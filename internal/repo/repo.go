// Package repo provides root-confined repository file operations.
package repo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxFileBytes          int64 = 1 << 20
	maxPatchBytes               = 256 << 10
	maxQueryBytes               = 512
	maxMatches                  = 100
	maxMatchTextBytes           = 4 << 10
	maxSearchOutputBytes        = 256 << 10
	maxSnapshotEntries          = 100_000
	defaultMaxSearchFiles       = 10_000
	defaultMaxSearchBytes int64 = 64 << 20
	maxSnapshotBytes      int64 = 2 << 30
)

var (
	// ErrRejected is deliberately non-specific so callers can return a safe MCP
	// error without disclosing paths, filesystem layout, or file contents.
	ErrRejected = errors.New("repository request rejected")
	ErrConfig   = errors.New("invalid repository root")
	errStopWalk = errors.New("stop repository walk")
	hunkHeader  = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@$`)
)

// Workspace confines every operation to one canonical repository root.
type Workspace struct {
	root           string
	rootDir        *os.File
	rootIdentity   string
	mu             sync.RWMutex
	maxSearchFiles int
	maxSearchBytes int64
}

// Match is one literal search result. Path is always relative to the
// configured repository root.
type Match struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SearchResult contains the bounded set of literal search matches.
type SearchResult struct {
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated"`
}

type SnapshotEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Digest string `json:"digest,omitempty"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
}

type SnapshotManifest struct {
	SnapshotID string          `json:"snapshot_id"`
	Entries    []SnapshotEntry `json:"entries"`
}

// New constructs a workspace rooted at an explicit absolute directory. The
// root is canonicalized once so later containment checks have one stable base.
func New(root string) (*Workspace, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, ErrConfig
	}

	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, ErrConfig
	}
	if containsGitDirectory(canonicalRoot) {
		return nil, ErrConfig
	}
	rootFD, err := unix.Open(canonicalRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrConfig
	}
	rootDir := os.NewFile(uintptr(rootFD), canonicalRoot)
	if rootDir == nil {
		_ = unix.Close(rootFD)
		return nil, ErrConfig
	}
	info, err := rootDir.Stat()
	if err != nil || !info.IsDir() {
		rootDir.Close()
		return nil, ErrConfig
	}
	identity, err := filesystemIdentity(rootDir)
	if err != nil {
		rootDir.Close()
		return nil, ErrConfig
	}

	return &Workspace{
		root:           filepath.Clean(canonicalRoot),
		rootDir:        rootDir,
		rootIdentity:   identity,
		maxSearchFiles: defaultMaxSearchFiles,
		maxSearchBytes: defaultMaxSearchBytes,
	}, nil
}

// RootIdentity returns the immutable filesystem identity captured from the
// opened repository root at startup. The canonical pathname is intentionally
// not exposed through this capability identifier.
func (w *Workspace) RootIdentity() string {
	return w.rootIdentity
}

// StartupPath is diagnostic metadata only; authorization uses the opened root handle.
func (w *Workspace) StartupPath() string {
	return w.root
}

// DuplicateRoot returns a duplicate of the opened repository directory capability.
func (w *Workspace) DuplicateRoot() (*os.File, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.rootDir == nil {
		return nil, ErrRejected
	}
	fd, err := unix.FcntlInt(w.rootDir.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrRejected
	}
	file := os.NewFile(uintptr(fd), "repository-root")
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrRejected
	}
	return file, nil
}

// Close releases the repository capability. Repeated calls are safe.
func (w *Workspace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rootDir == nil {
		return nil
	}
	err := w.rootDir.Close()
	w.rootDir = nil
	return err
}

func filesystemIdentity(file *os.File) (string, error) {
	if file == nil {
		return "", ErrConfig
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return "", ErrConfig
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))))
	return hex.EncodeToString(digest[:]), nil
}

func rootDevice(file *os.File) (uint64, error) {
	if file == nil {
		return 0, ErrConfig
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, ErrConfig
	}
	return uint64(stat.Dev), nil
}

func (w *Workspace) openExistingRelative(input string, allowRoot bool) (*os.File, string, error) {
	cleanPath, err := cleanRelativePath(input)
	if err != nil || (!allowRoot && cleanPath == ".") || isProtected(cleanPath) {
		return nil, "", ErrRejected
	}
	rootDev, err := rootDevice(w.rootDir)
	if err != nil {
		return nil, "", ErrRejected
	}
	// Open a fresh descriptor for every traversal. Duplicating rootDir would
	// share its open-file-description and therefore its directory offset with
	// other walks, making repeated snapshots/searches nondeterministic.
	currentFD, err := unix.Openat(int(w.rootDir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", ErrRejected
	}
	if cleanPath == "." {
		return os.NewFile(uintptr(currentFD), "."), ".", nil
	}
	components := strings.Split(filepath.ToSlash(cleanPath), "/")
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		nextFD, err := unix.Openat(currentFD, component, flags, 0)
		_ = unix.Close(currentFD)
		if err != nil {
			return nil, "", ErrRejected
		}
		currentFD = nextFD
		var stat unix.Stat_t
		if err := unix.Fstat(currentFD, &stat); err != nil || uint64(stat.Dev) != rootDev {
			_ = unix.Close(currentFD)
			return nil, "", ErrRejected
		}
	}
	file := os.NewFile(uintptr(currentFD), filepath.ToSlash(cleanPath))
	if file == nil {
		_ = unix.Close(currentFD)
		return nil, "", ErrRejected
	}
	return file, filepath.ToSlash(cleanPath), nil
}

// Read returns one UTF-8 text file. It rejects protected, non-regular, large,
// and escaped files.
func (w *Workspace) Read(path string) (string, string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	file, relativePath, err := w.openExistingRelative(path, false)
	if err != nil {
		return "", "", ErrRejected
	}
	defer file.Close()

	content, err := readTextOpenFile(file)
	if err != nil {
		return "", "", ErrRejected
	}
	return relativePath, content, nil
}

type searchAccumulator struct {
	result       SearchResult
	filesScanned int
	bytesScanned int64
	outputBytes  int
}

func (w *Workspace) searchOpenedFile(query string, file *os.File, relativePath string, acc *searchAccumulator) error {
	info, err := file.Stat()
	if err != nil {
		return ErrRejected
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > maxFileBytes {
		return nil
	}
	if acc.filesScanned >= w.maxSearchFiles || info.Size() > w.maxSearchBytes-acc.bytesScanned {
		acc.result.Truncated = true
		return errStopWalk
	}
	acc.filesScanned++
	acc.bytesScanned += info.Size()

	matches, used, truncated, err := searchTextOpenFile(
		query,
		file,
		relativePath,
		maxMatches-len(acc.result.Matches),
		maxSearchOutputBytes-acc.outputBytes,
	)
	if err != nil {
		return nil
	}
	acc.result.Matches = append(acc.result.Matches, matches...)
	acc.outputBytes += used
	if truncated || len(acc.result.Matches) == maxMatches || acc.outputBytes >= maxSearchOutputBytes {
		acc.result.Truncated = true
		return errStopWalk
	}
	return nil
}

func (w *Workspace) searchDirectory(ctx context.Context, query string, dir *os.File, relativeDir string, acc *searchAccumulator) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return ErrRejected
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ErrRejected
		default:
		}
		relativePath := entry.Name()
		if relativeDir != "." {
			relativePath = relativeDir + "/" + entry.Name()
		}
		if isProtected(relativePath) || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		child, _, err := w.openExistingRelative(relativePath, true)
		if err != nil {
			return ErrRejected
		}
		info, err := child.Stat()
		if err != nil {
			child.Close()
			return ErrRejected
		}
		if info.IsDir() {
			err = w.searchDirectory(ctx, query, child, relativePath, acc)
			child.Close()
			if err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			child.Close()
			continue
		}
		err = w.searchOpenedFile(query, child, relativePath, acc)
		child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Search performs a bounded literal text search. It never follows symlinks
// while walking and skips all protected, binary, and oversized files.
func (w *Workspace) Search(ctx context.Context, query, scope string) (SearchResult, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if ctx == nil || query == "" || len(query) > maxQueryBytes || !utf8.ValidString(query) {
		return SearchResult{}, ErrRejected
	}
	select {
	case <-ctx.Done():
		return SearchResult{}, ErrRejected
	default:
	}

	scopePath := "."
	if scope != "" {
		scopePath = scope
	}
	file, relativePath, err := w.openExistingRelative(scopePath, true)
	if err != nil {
		return SearchResult{}, ErrRejected
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return SearchResult{}, ErrRejected
	}

	acc := &searchAccumulator{}
	if info.IsDir() {
		err = w.searchDirectory(ctx, query, file, relativePath, acc)
	} else {
		err = w.searchOpenedFile(query, file, relativePath, acc)
	}
	if err != nil && !errors.Is(err, errStopWalk) {
		return SearchResult{}, ErrRejected
	}
	return acc.result, nil
}

func digestOpenFile(file *os.File) (string, error) {
	if file == nil {
		return "", ErrRejected
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", ErrRejected
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", ErrRejected
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type snapshotAccumulator struct {
	entries []SnapshotEntry
	bytes   int64
}

// Snapshot helpers are deterministic and repository-relative.

func snapshotManifestID(entries []SnapshotEntry) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, "m3-snapshot-v1\n")
	for _, entry := range entries {
		_, _ = fmt.Fprintf(hasher, "%d:%s|%d:%s|%d:%s|%d|%d\n", len(entry.Path), entry.Path, len(entry.Type), entry.Type, len(entry.Digest), entry.Digest, entry.Mode, entry.Size)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
func (w *Workspace) snapshotDirectory(ctx context.Context, dir *os.File, relativeDir string, acc *snapshotAccumulator) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return ErrRejected
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ErrRejected
		default:
		}
		relativePath := entry.Name()
		if relativeDir != "." {
			relativePath = relativeDir + "/" + entry.Name()
		}
		if isProtected(relativePath) {
			continue
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return ErrRejected
		}
		child, _, err := w.openExistingRelative(relativePath, true)
		if err != nil {
			return ErrRejected
		}
		info, err := child.Stat()
		if err != nil {
			child.Close()
			return ErrRejected
		}
		if len(acc.entries) >= maxSnapshotEntries {
			child.Close()
			return ErrRejected
		}
		if info.IsDir() {
			acc.entries = append(acc.entries, SnapshotEntry{Path: relativePath, Type: "directory", Mode: uint32(info.Mode().Perm())})
			err = w.snapshotDirectory(ctx, child, relativePath, acc)
			child.Close()
			if err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSnapshotBytes-acc.bytes {
			child.Close()
			return ErrRejected
		}
		digest, err := digestOpenFile(child)
		child.Close()
		if err != nil {
			return ErrRejected
		}
		acc.bytes += info.Size()
		acc.entries = append(acc.entries, SnapshotEntry{
			Path: relativePath, Type: "regular", Digest: digest,
			Mode: uint32(info.Mode().Perm()), Size: info.Size(),
		})
	}
	return nil
}

// Snapshot returns a deterministic manifest of the permitted repository tree.
func (w *Workspace) Snapshot(ctx context.Context) (SnapshotManifest, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if ctx == nil {
		return SnapshotManifest{}, ErrRejected
	}
	root, _, err := w.openExistingRelative(".", true)
	if err != nil {
		return SnapshotManifest{}, ErrRejected
	}
	defer root.Close()
	acc := &snapshotAccumulator{}
	if err := w.snapshotDirectory(ctx, root, ".", acc); err != nil {
		return SnapshotManifest{}, ErrRejected
	}
	return SnapshotManifest{SnapshotID: snapshotManifestID(acc.entries), Entries: acc.entries}, nil
}

// ApplyPatch applies one strict, single-file unified diff to existing repository text
// file. Every hunk must match exactly; the file is atomically replaced only
// after all validation succeeds. Mutations are serialized so concurrent exact-
// context patches cannot both commit from the same stale snapshot.
func (w *Workspace) ApplyPatch(patch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(patch) == 0 || len(patch) > maxPatchBytes || !utf8.ValidString(patch) {
		return "", ErrRejected
	}

	filePatch, err := parsePatch(patch)
	if err != nil {
		return "", ErrRejected
	}
	target, relativePath, err := w.openExistingRelative(filePatch.path, false)
	if err != nil {
		return "", ErrRejected
	}
	defer target.Close()
	targetStat, err := fdStat(target)
	if err != nil {
		return "", ErrRejected
	}

	info, err := target.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrRejected
	}
	content, err := readTextOpenFile(target)
	if err != nil {
		return "", ErrRejected
	}
	updated, err := applyHunks(content, filePatch.hunks)
	if err != nil || updated == content {
		return "", ErrRejected
	}
	parent, baseName, err := w.openParentDirectory(filePatch.path)
	if err != nil {
		return "", ErrRejected
	}
	defer parent.Close()
	if err := writeAtomicAt(parent, baseName, []byte(updated), info.Mode().Perm(), targetStat, content); err != nil {
		return "", ErrRejected
	}
	return relativePath, nil
}

func fdStat(file *os.File) (unix.Stat_t, error) {
	if file == nil {
		return unix.Stat_t{}, ErrRejected
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return unix.Stat_t{}, ErrRejected
	}
	return stat, nil
}

func (w *Workspace) openParentDirectory(input string) (*os.File, string, error) {
	cleanPath, err := cleanRelativePath(input)
	if err != nil || cleanPath == "." || isProtected(cleanPath) {
		return nil, "", ErrRejected
	}
	slashPath := filepath.ToSlash(cleanPath)
	parentPath := "."
	baseName := slashPath
	if index := strings.LastIndex(slashPath, "/"); index >= 0 {
		parentPath = slashPath[:index]
		baseName = slashPath[index+1:]
	}
	if baseName == "" || baseName == "." || strings.Contains(baseName, "/") {
		return nil, "", ErrRejected
	}
	parent, _, err := w.openExistingRelative(parentPath, true)
	if err != nil {
		return nil, "", ErrRejected
	}
	info, err := parent.Stat()
	if err != nil || !info.IsDir() {
		parent.Close()
		return nil, "", ErrRejected
	}
	return parent, baseName, nil
}

func sameFileIdentity(left, right unix.Stat_t) bool {
	return uint64(left.Dev) == uint64(right.Dev) && uint64(left.Ino) == uint64(right.Ino)
}

func randomTempName() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", ErrRejected
	}
	return ".repoworker-" + hex.EncodeToString(buffer), nil
}

func createTempAt(parent *os.File, mode fs.FileMode) (*os.File, string, error) {
	if parent == nil {
		return nil, "", ErrRejected
	}
	for attempts := 0; attempts < 8; attempts++ {
		name, err := randomTempName()
		if err != nil {
			return nil, "", ErrRejected
		}
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, uint32(mode.Perm()))
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return nil, "", ErrRejected
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			return nil, "", ErrRejected
		}
		return file, name, nil
	}
	return nil, "", ErrRejected
}

func verifyPreimageAt(parent *os.File, baseName string, original unix.Stat_t, originalContent string, mode fs.FileMode) error {
	fd, err := unix.Openat(int(parent.Fd()), baseName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return ErrRejected
	}
	file := os.NewFile(uintptr(fd), baseName)
	if file == nil {
		_ = unix.Close(fd)
		return ErrRejected
	}
	defer file.Close()
	stat, err := fdStat(file)
	if err != nil || !sameFileIdentity(stat, original) || fs.FileMode(stat.Mode).Perm() != mode.Perm() {
		return ErrRejected
	}
	content, err := readTextOpenFile(file)
	if err != nil || content != originalContent {
		return ErrRejected
	}
	return nil
}

func writeAtomicAt(parent *os.File, baseName string, content []byte, mode fs.FileMode, original unix.Stat_t, originalContent string) error {
	temporary, temporaryName, err := createTempAt(parent, mode)
	if err != nil {
		return ErrRejected
	}
	created := true
	defer func() {
		if created {
			_ = unix.Unlinkat(int(parent.Fd()), temporaryName, 0)
		}
	}()
	if written, err := temporary.Write(content); err != nil || written != len(content) {
		temporary.Close()
		return ErrRejected
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
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
	if err := verifyPreimageAt(parent, baseName, original, originalContent, mode); err != nil {
		return ErrRejected
	}
	if err := unix.Renameat(int(parent.Fd()), temporaryName, int(parent.Fd()), baseName); err != nil {
		return ErrRejected
	}
	created = false
	if err := parent.Sync(); err != nil {
		return ErrRejected
	}
	return nil
}

// CreateFile creates one new UTF-8 text file without overwriting an existing path.
func (w *Workspace) CreateFile(path, content string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(content) > int(maxFileBytes) || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return "", ErrRejected
	}
	cleanPath, err := cleanRelativePath(path)
	if err != nil || cleanPath == "." || isProtected(cleanPath) {
		return "", ErrRejected
	}
	parent, baseName, err := w.openParentDirectory(cleanPath)
	if err != nil {
		return "", ErrRejected
	}
	defer parent.Close()
	temporary, temporaryName, err := createTempAt(parent, 0o644)
	if err != nil {
		return "", ErrRejected
	}
	staged := true
	defer func() {
		if staged {
			_ = unix.Unlinkat(int(parent.Fd()), temporaryName, 0)
		}
	}()
	if written, err := temporary.Write([]byte(content)); err != nil || written != len(content) {
		temporary.Close()
		return "", ErrRejected
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return "", ErrRejected
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", ErrRejected
	}
	if err := temporary.Close(); err != nil {
		return "", ErrRejected
	}
	if err := unix.Linkat(int(parent.Fd()), temporaryName, int(parent.Fd()), baseName, 0); err != nil {
		return "", ErrRejected
	}
	if err := unix.Unlinkat(int(parent.Fd()), temporaryName, 0); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), baseName, 0)
		return "", ErrRejected
	}
	staged = false
	if err := parent.Sync(); err != nil {
		return "", ErrRejected
	}
	return filepath.ToSlash(cleanPath), nil
}

// DeleteFile removes one existing permitted regular file without following symlinks.
func (w *Workspace) DeleteFile(path string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	cleanPath, err := cleanRelativePath(path)
	if err != nil || cleanPath == "." || isProtected(cleanPath) {
		return "", ErrRejected
	}
	parent, baseName, err := w.openParentDirectory(cleanPath)
	if err != nil {
		return "", ErrRejected
	}
	defer parent.Close()
	rootDev, err := rootDevice(w.rootDir)
	if err != nil {
		return "", ErrRejected
	}
	fd, err := unix.Openat(int(parent.Fd()), baseName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", ErrRejected
	}
	target := os.NewFile(uintptr(fd), baseName)
	if target == nil {
		_ = unix.Close(fd)
		return "", ErrRejected
	}
	stat, err := fdStat(target)
	info, infoErr := target.Stat()
	if closeErr := target.Close(); closeErr != nil {
		return "", ErrRejected
	}
	if err != nil || infoErr != nil || !info.Mode().IsRegular() || uint64(stat.Dev) != rootDev {
		return "", ErrRejected
	}
	if err := unix.Unlinkat(int(parent.Fd()), baseName, 0); err != nil {
		return "", ErrRejected
	}
	if err := parent.Sync(); err != nil {
		return "", ErrRejected
	}
	return filepath.ToSlash(cleanPath), nil
}

func cleanRelativePath(input string) (string, error) {
	if input == "" || strings.ContainsRune(input, 0) || filepath.IsAbs(input) || strings.Contains(input, "\\") || isWindowsVolumePath(input) {
		return "", ErrRejected
	}
	for _, component := range strings.Split(filepath.ToSlash(input), "/") {
		if component == ".." {
			return "", ErrRejected
		}
	}
	cleanPath := filepath.Clean(input)
	if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", ErrRejected
	}
	return cleanPath, nil
}

func isWindowsVolumePath(path string) bool {
	return len(path) >= 2 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) && path[1] == ':'
}

func containsGitDirectory(path string) bool {
	for _, component := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if component == ".git" {
			return true
		}
	}
	return false
}

func isProtected(relativePath string) bool {
	if relativePath == "." {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(relativePath), "/") {
		name := strings.ToLower(component)
		if name == ".git" || name == ".env" || strings.HasPrefix(name, ".env.") {
			return true
		}
		switch name {
		case ".netrc", ".npmrc", ".pypirc", ".ssh", ".aws", ".gnupg",
			"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "private_key", "privatekey", "authorized_keys",
			"credentials.json", "credentials.yaml", "credentials.yml",
			"secret.json", "secret.yaml", "secret.yml", "secrets.json", "secrets.yaml", "secrets.yml",
			"token.json", "token.yaml", "token.yml", "tokens.json", "tokens.yaml", "tokens.yml":
			return true
		}
		extension := strings.ToLower(filepath.Ext(name))
		if extension == ".key" || extension == ".pem" || extension == ".p12" || extension == ".pfx" || extension == ".token" {
			return true
		}
		if containsSensitiveKeyword(name) && !isSourceCodePath(name) {
			return true
		}
	}
	return false
}

func containsSensitiveKeyword(name string) bool {
	return strings.Contains(name, "credential") || strings.Contains(name, "secret") || strings.Contains(name, "token")
}

func isSourceCodePath(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".c", ".cc", ".cpp", ".cs", ".go", ".h", ".hpp", ".java", ".js", ".jsx", ".kt", ".kts", ".m", ".mm", ".php", ".py", ".rb", ".rs", ".scala", ".sh", ".swift", ".ts", ".tsx", ".zsh":
		return true
	default:
		return false
	}
}

func readTextOpenFile(file *os.File) (string, error) {
	if file == nil {
		return "", ErrRejected
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", ErrRejected
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil || int64(len(content)) > maxFileBytes || !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return "", ErrRejected
	}
	return string(content), nil
}

func searchTextOpenFile(query string, file *os.File, relativePath string, matchLimit, outputLimit int) ([]Match, int, bool, error) {
	if matchLimit <= 0 || outputLimit <= 0 {
		return nil, 0, true, nil
	}
	content, err := readTextOpenFile(file)
	if err != nil {
		return nil, 0, false, err
	}
	lines := strings.Split(content, "\n")
	matches := make([]Match, 0)
	used := 0
	for index, line := range lines {
		if !strings.Contains(line, query) {
			continue
		}
		preview, previewTruncated := matchPreview(line, query)
		match := Match{Path: relativePath, Line: index + 1, Text: preview, Truncated: previewTruncated}
		cost := len(match.Path) + len(match.Text) + 64
		if len(matches) == matchLimit || cost > outputLimit-used {
			return matches, used, true, nil
		}
		matches = append(matches, match)
		used += cost
	}
	return matches, used, false, nil
}

func matchPreview(line, query string) (string, bool) {
	if len(line) <= maxMatchTextBytes {
		return line, false
	}
	matchIndex := strings.Index(line, query)
	if matchIndex < 0 {
		matchIndex = 0
	}
	start := matchIndex - (maxMatchTextBytes-len(query))/2
	if start < 0 {
		start = 0
	}
	end := start + maxMatchTextBytes
	if end > len(line) {
		end = len(line)
		start = end - maxMatchTextBytes
		if start < 0 {
			start = 0
		}
	}
	for start < end && !utf8.RuneStart(line[start]) {
		start++
	}
	for end > start && !utf8.ValidString(line[start:end]) {
		end--
	}
	return line[start:end], true
}

type patch struct {
	path  string
	hunks []hunk
}

type hunk struct {
	oldStart int
	oldCount int
	lines    []patchLine
}

type patchLine struct {
	kind byte
	text string
}

func parsePatch(input string) (patch, error) {
	if !strings.HasSuffix(input, "\n") {
		return patch{}, ErrRejected
	}
	lines := strings.Split(strings.TrimSuffix(input, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "--- ") || !strings.HasPrefix(lines[1], "+++ ") {
		return patch{}, ErrRejected
	}
	oldPath, err := patchPath(strings.TrimPrefix(lines[0], "--- "), "a/")
	if err != nil {
		return patch{}, ErrRejected
	}
	newPath, err := patchPath(strings.TrimPrefix(lines[1], "+++ "), "b/")
	if err != nil || oldPath != newPath {
		return patch{}, ErrRejected
	}

	result := patch{path: oldPath}
	for index := 2; index < len(lines); {
		match := hunkHeader.FindStringSubmatch(lines[index])
		if match == nil {
			return patch{}, ErrRejected
		}
		oldStart, oldCount, err := hunkRange(match[1], match[2])
		if err != nil || oldStart == 0 {
			return patch{}, ErrRejected
		}
		newStart, newCount, err := hunkRange(match[3], match[4])
		if err != nil || newStart == 0 {
			return patch{}, ErrRejected
		}
		index++
		current := hunk{oldStart: oldStart, oldCount: oldCount}
		oldLines, newLines := 0, 0
		for index < len(lines) && !strings.HasPrefix(lines[index], "@@ ") {
			if len(lines[index]) == 0 || (lines[index][0] != ' ' && lines[index][0] != '+' && lines[index][0] != '-') {
				return patch{}, ErrRejected
			}
			line := patchLine{kind: lines[index][0], text: lines[index][1:]}
			current.lines = append(current.lines, line)
			if line.kind != '+' {
				oldLines++
			}
			if line.kind != '-' {
				newLines++
			}
			index++
		}
		if len(current.lines) == 0 || oldLines != current.oldCount || newLines != newCount {
			return patch{}, ErrRejected
		}
		result.hunks = append(result.hunks, current)
	}
	return result, nil
}

func patchPath(headerPath, expectedPrefix string) (string, error) {
	if strings.ContainsAny(headerPath, "\t\r") || !strings.HasPrefix(headerPath, expectedPrefix) {
		return "", ErrRejected
	}
	path, err := cleanRelativePath(strings.TrimPrefix(headerPath, expectedPrefix))
	if err != nil || path == "." || isProtected(path) {
		return "", ErrRejected
	}
	return filepath.ToSlash(path), nil
}

func hunkRange(startText, countText string) (int, int, error) {
	start, err := strconv.Atoi(startText)
	if err != nil || start < 0 {
		return 0, 0, ErrRejected
	}
	if countText == "" {
		return start, 1, nil
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 0 {
		return 0, 0, ErrRejected
	}
	return start, count, nil
}

func applyHunks(content string, hunks []hunk) (string, error) {
	if !strings.HasSuffix(content, "\n") {
		return "", ErrRejected
	}
	original := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "\n" {
		original = []string{""}
	}
	updated := make([]string, 0, len(original))
	cursor := 0
	for _, hunk := range hunks {
		start := hunk.oldStart - 1
		if start < cursor || start > len(original) {
			return "", ErrRejected
		}
		updated = append(updated, original[cursor:start]...)

		position := start
		oldLines := 0
		for _, line := range hunk.lines {
			if line.kind != '+' {
				if position >= len(original) || original[position] != line.text {
					return "", ErrRejected
				}
				position++
				oldLines++
			}
			if line.kind != '-' {
				updated = append(updated, line.text)
			}
		}
		if oldLines != hunk.oldCount {
			return "", ErrRejected
		}
		cursor = position
	}
	updated = append(updated, original[cursor:]...)
	return strings.Join(updated, "\n") + "\n", nil
}
