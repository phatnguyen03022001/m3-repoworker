package repo

import (
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strings"
)

// GitStatus is a read-only, typed summary bound to the opened repository
// descriptor. It intentionally returns identities rather than host paths.
type GitStatus struct {
	RepositoryIdentity string   `json:"repository_identity"`
	TrustedRoot        string   `json:"trusted_root"`
	Branch             string   `json:"branch"`
	Head               string   `json:"head"`
	Dirty              bool     `json:"dirty"`
	ChangedPaths       []string `json:"changed_paths"`
}

func (w *Workspace) GitStatus(ctx context.Context) (GitStatus, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if ctx == nil || w == nil || w.rootDir == nil || w.root == "" {
		return GitStatus{}, ErrRejected
	}
	if identity, err := filesystemIdentity(w.rootDir); err != nil || identity != w.rootIdentity {
		return GitStatus{}, ErrRejected
	}
	branch, err := runGitStatusCommand(ctx, w.root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) == "" {
		// Detached and unborn repositories fail closed by policy.
		return GitStatus{}, ErrRejected
	}
	branchName := strings.TrimSpace(string(branch))
	head, err := runGitStatusCommand(ctx, w.root, "rev-parse", "--verify", "HEAD")
	if err != nil || len(strings.TrimSpace(string(head))) != 40 {
		return GitStatus{}, ErrRejected
	}
	headName := strings.TrimSpace(string(head))
	for _, char := range headName {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return GitStatus{}, ErrRejected
		}
	}
	output, err := runGitStatusCommand(ctx, w.root, "status", "--porcelain=v1", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return GitStatus{}, ErrRejected
	}
	changed, err := parsePorcelainStatus(output)
	if err != nil {
		return GitStatus{}, ErrRejected
	}
	return GitStatus{RepositoryIdentity: w.rootIdentity, TrustedRoot: w.rootIdentity, Branch: branchName, Head: strings.ToLower(headName), Dirty: len(changed) != 0, ChangedPaths: changed}, nil
}

func runGitStatusCommand(ctx context.Context, root string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", root, "--no-optional-locks"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = []string{"GIT_OPTIONAL_LOCKS=0", "LC_ALL=C", "PATH=/usr/bin:/bin:/opt/homebrew/bin"}
	output, err := command.Output()
	if err != nil {
		return nil, ErrRejected
	}
	return output, nil
}

func parsePorcelainStatus(output []byte) ([]string, error) {
	if len(output) == 0 {
		return []string{}, nil
	}
	parts := bytes.Split(output, []byte{0})
	if len(parts) == 0 || !bytes.HasPrefix(parts[0], []byte("## ")) {
		return nil, ErrRejected
	}
	paths := make([]string, 0, len(parts))
	for index := 1; index < len(parts); index++ {
		part := parts[index]
		if len(part) == 0 {
			continue
		}
		if len(part) < 4 || part[2] != ' ' {
			return nil, ErrRejected
		}
		status := string(part[:2])
		path := string(part[3:])
		if path == "" || strings.ContainsAny(path, "\x00\r\n") {
			return nil, ErrRejected
		}
		clean, err := cleanRelativePath(path)
		if err != nil || clean == "." || isUnavailable(clean) {
			return nil, ErrRejected
		}
		paths = append(paths, status+" "+strings.ReplaceAll(clean, "\\", "/"))
		if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
			index++
			if index >= len(parts) || len(parts[index]) == 0 {
				return nil, ErrRejected
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}
