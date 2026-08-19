// Package repo provides root-confined repository file operations.
package repo

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxFileBytes          int64 = 1 << 20
	maxPatchBytes               = 256 << 10
	maxQueryBytes               = 512
	maxMatches                  = 100
	maxMatchTextBytes           = 4 << 10
	maxSearchOutputBytes        = 256 << 10
	defaultMaxSearchFiles       = 10_000
	defaultMaxSearchBytes int64 = 64 << 20
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
	info, err := os.Stat(canonicalRoot)
	if err != nil || !info.IsDir() {
		return nil, ErrConfig
	}

	return &Workspace{
		root:           filepath.Clean(canonicalRoot),
		maxSearchFiles: defaultMaxSearchFiles,
		maxSearchBytes: defaultMaxSearchBytes,
	}, nil
}

// Read returns one UTF-8 text file. It rejects protected, non-regular, large,
// and escaped files.
func (w *Workspace) Read(path string) (string, string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	target, relativePath, err := w.resolveExisting(path, false)
	if err != nil {
		return "", "", ErrRejected
	}
	content, err := readTextFile(target)
	if err != nil {
		return "", "", ErrRejected
	}
	return relativePath, content, nil
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

	searchRoot := w.root
	if scope != "" {
		resolved, _, err := w.resolveExisting(scope, true)
		if err != nil {
			return SearchResult{}, ErrRejected
		}
		searchRoot = resolved
	}

	info, err := os.Stat(searchRoot)
	if err != nil {
		return SearchResult{}, ErrRejected
	}
	if !info.IsDir() {
		return w.searchFile(query, searchRoot)
	}

	result := SearchResult{}
	filesScanned := 0
	var bytesScanned int64
	outputBytes := 0
	err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		select {
		case <-ctx.Done():
			return ErrRejected
		default:
		}
		if walkErr != nil {
			return ErrRejected
		}
		relativePath, err := w.relative(path)
		if err != nil {
			return ErrRejected
		}
		if relativePath != "." && isProtected(relativePath) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		entryInfo, err := entry.Info()
		if err != nil || entryInfo.Size() > maxFileBytes {
			return nil
		}
		if filesScanned >= w.maxSearchFiles || entryInfo.Size() > w.maxSearchBytes-bytesScanned {
			result.Truncated = true
			return errStopWalk
		}
		filesScanned++
		bytesScanned += entryInfo.Size()

		matches, used, truncated, err := searchTextFile(
			query,
			path,
			relativePath,
			maxMatches-len(result.Matches),
			maxSearchOutputBytes-outputBytes,
		)
		if err != nil {
			return nil // Unsupported file types and unreadable files are skipped.
		}
		result.Matches = append(result.Matches, matches...)
		outputBytes += used
		if truncated || len(result.Matches) == maxMatches || outputBytes >= maxSearchOutputBytes {
			result.Truncated = true
			return errStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return SearchResult{}, ErrRejected
	}
	return result, nil
}

func (w *Workspace) searchFile(query, path string) (SearchResult, error) {
	relativePath, err := w.relative(path)
	if err != nil || isProtected(relativePath) {
		return SearchResult{}, ErrRejected
	}
	matches, _, truncated, err := searchTextFile(query, path, relativePath, maxMatches, maxSearchOutputBytes)
	if err != nil {
		return SearchResult{}, ErrRejected
	}
	return SearchResult{Matches: matches, Truncated: truncated}, nil
}

// ApplyPatch applies one strict, single-file unified diff to an existing text
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
	target, relativePath, err := w.resolveExisting(filePatch.path, false)
	if err != nil || hasSymlinkComponent(w.root, filePatch.path) {
		return "", ErrRejected
	}

	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrRejected
	}
	content, err := readTextFile(target)
	if err != nil {
		return "", ErrRejected
	}
	updated, err := applyHunks(content, filePatch.hunks)
	if err != nil || updated == content {
		return "", ErrRejected
	}
	if err := writeAtomic(target, []byte(updated), info.Mode().Perm()); err != nil {
		return "", ErrRejected
	}
	return relativePath, nil
}

func (w *Workspace) resolveExisting(input string, allowRoot bool) (string, string, error) {
	cleanPath, err := cleanRelativePath(input)
	if err != nil || (!allowRoot && cleanPath == ".") || isProtected(cleanPath) {
		return "", "", ErrRejected
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(w.root, cleanPath))
	if err != nil {
		return "", "", ErrRejected
	}
	relativePath, err := w.relative(resolved)
	if err != nil || isProtected(relativePath) {
		return "", "", ErrRejected
	}
	return resolved, relativePath, nil
}

func (w *Workspace) relative(path string) (string, error) {
	relativePath, err := filepath.Rel(w.root, path)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || filepath.IsAbs(relativePath) {
		return "", ErrRejected
	}
	return filepath.ToSlash(filepath.Clean(relativePath)), nil
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

func readTextFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", ErrRejected
	}
	content, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return "", ErrRejected
	}
	return string(content), nil
}

func searchTextFile(query, path, relativePath string, matchLimit, outputLimit int) ([]Match, int, bool, error) {
	if matchLimit <= 0 || outputLimit <= 0 {
		return nil, 0, true, nil
	}
	content, err := readTextFile(path)
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

func hasSymlinkComponent(root, relativePath string) bool {
	current := root
	for _, component := range strings.Split(filepath.FromSlash(relativePath), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
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
		hasContext := false
		for index < len(lines) && !strings.HasPrefix(lines[index], "@@ ") {
			if len(lines[index]) == 0 || (lines[index][0] != ' ' && lines[index][0] != '+' && lines[index][0] != '-') {
				return patch{}, ErrRejected
			}
			line := patchLine{kind: lines[index][0], text: lines[index][1:]}
			current.lines = append(current.lines, line)
			if line.kind == ' ' {
				hasContext = true
			}
			if line.kind != '+' {
				oldLines++
			}
			if line.kind != '-' {
				newLines++
			}
			index++
		}
		if len(current.lines) == 0 || !hasContext || oldLines != current.oldCount || newLines != newCount {
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

func writeAtomic(path string, content []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".repoworker-*")
	if err != nil {
		return ErrRejected
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return ErrRejected
	}
	if err := temporary.Chmod(mode); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return ErrRejected
	}
	return nil
}
