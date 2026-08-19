// Package repo provides root-confined repository file operations.
package repo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxFileBytes  int64 = 1 << 20
	maxPatchBytes       = 256 << 10
	maxQueryBytes       = 512
	maxMatches          = 100
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
	root string
}

// Match is one literal search result. Path is always relative to the
// configured repository root.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
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

	return &Workspace{root: filepath.Clean(canonicalRoot)}, nil
}

// Read returns one UTF-8 text file. It rejects protected, non-regular, large,
// and escaped files.
func (w *Workspace) Read(path string) (string, string, error) {
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
func (w *Workspace) Search(query, scope string) (SearchResult, error) {
	if query == "" || len(query) > maxQueryBytes || !utf8.ValidString(query) {
		return SearchResult{}, ErrRejected
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
	err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
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

		matches, err := searchTextFile(query, path, relativePath)
		if err != nil {
			return nil // Unsupported file types and unreadable files are skipped.
		}
		for _, match := range matches {
			if len(result.Matches) == maxMatches {
				result.Truncated = true
				return errStopWalk
			}
			result.Matches = append(result.Matches, match)
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
	matches, err := searchTextFile(query, path, relativePath)
	if err != nil {
		return SearchResult{}, ErrRejected
	}
	result := SearchResult{Matches: matches}
	if len(result.Matches) > maxMatches {
		result.Matches = result.Matches[:maxMatches]
		result.Truncated = true
	}
	return result, nil
}

// ApplyPatch applies one strict, single-file unified diff to an existing text
// file. Every hunk must match exactly; the file is atomically replaced only
// after all validation succeeds.
func (w *Workspace) ApplyPatch(patch string) (string, error) {
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
		if strings.Contains(name, "credential") || strings.Contains(name, "secret") || strings.Contains(name, "token") {
			return true
		}
		switch name {
		case ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "private_key", "privatekey", "authorized_keys":
			return true
		}
		extension := strings.ToLower(filepath.Ext(name))
		if extension == ".key" || extension == ".pem" || extension == ".p12" || extension == ".pfx" || extension == ".token" {
			return true
		}
	}
	return false
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

func searchTextFile(query, path, relativePath string) ([]Match, error) {
	content, err := readTextFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(content, "\n")
	matches := make([]Match, 0)
	for index, line := range lines {
		if strings.Contains(line, query) {
			matches = append(matches, Match{Path: relativePath, Line: index + 1, Text: line})
		}
	}
	return matches, nil
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
		start := hunk.oldStart
		if start > 0 {
			start--
		}
		if start < cursor || start > len(original) {
			return "", ErrRejected
		}
		updated = append(updated, original[cursor:start]...)
		oldLines := 0
		for _, line := range hunk.lines {
			if line.kind != '+' {
				if cursor+oldLines >= len(original) || original[cursor+oldLines] != line.text {
					return "", ErrRejected
				}
				oldLines++
			}
			if line.kind != '-' {
				updated = append(updated, line.text)
			}
		}
		if oldLines != hunk.oldCount {
			return "", ErrRejected
		}
		cursor += oldLines
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
