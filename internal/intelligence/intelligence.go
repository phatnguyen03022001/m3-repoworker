// Package intelligence detects repository ecosystems and creates native,
// read-only verification command plans without owning a build graph.
package intelligence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrRejected = errors.New("repository intelligence request rejected")

type Ecosystem string

const (
	EcosystemGo    Ecosystem = "go"
	EcosystemNode  Ecosystem = "node"
	EcosystemRust  Ecosystem = "rust"
	EcosystemNx    Ecosystem = "nx"
	EcosystemTurbo Ecosystem = "turbo"
	EcosystemBazel Ecosystem = "bazel"
)

type RepositoryInfo struct {
	Root           string      `json:"root"`
	Ecosystems     []Ecosystem `json:"ecosystems"`
	PackageManager string      `json:"package_manager,omitempty"`
}

type Target struct {
	Name     string
	Affected bool
}

type Command struct {
	Ecosystem  Ecosystem `json:"ecosystem"`
	Executable string    `json:"executable"`
	Arguments  []string  `json:"arguments"`
	Workdir    string    `json:"workdir"`
}

type VerificationPlan struct {
	RepositoryID      string    `json:"repository_id"`
	CandidateSnapshot string    `json:"candidate_snapshot"`
	EnvironmentID     string    `json:"environment_id"`
	PolicyVersion     string    `json:"policy_version"`
	Target            Target    `json:"target"`
	Commands          []Command `json:"commands"`
	PlanDigest        string    `json:"plan_digest"`
}

type VerificationResult struct {
	PlanDigest        string    `json:"plan_digest"`
	RepositoryID      string    `json:"repository_id"`
	CandidateSnapshot string    `json:"candidate_snapshot"`
	EnvironmentID     string    `json:"environment_id"`
	PolicyVersion     string    `json:"policy_version"`
	Passed            bool      `json:"passed"`
	ExitCode          int       `json:"exit_code"`
	Diagnostic        string    `json:"diagnostic,omitempty"`
	VerifiedAt        time.Time `json:"verified_at"`
}

type Runner interface {
	Run(context.Context, Command) (int, string)
}

type SnapshotProvider func(context.Context) (string, error)

func Detect(ctx context.Context, root string) (RepositoryInfo, error) {
	if ctx == nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return RepositoryInfo{}, ErrRejected
	}
	checks := []struct {
		name      string
		ecosystem Ecosystem
	}{
		{"go.mod", EcosystemGo}, {"package.json", EcosystemNode}, {"Cargo.toml", EcosystemRust},
		{"nx.json", EcosystemNx}, {"turbo.json", EcosystemTurbo}, {"MODULE.bazel", EcosystemBazel}, {"WORKSPACE", EcosystemBazel},
	}
	seen := map[Ecosystem]bool{}
	info := RepositoryInfo{Root: root}
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			return RepositoryInfo{}, err
		}
		stat, err := os.Lstat(filepath.Join(root, check.name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !stat.Mode().IsRegular() || stat.Mode()&os.ModeSymlink != 0 {
			return RepositoryInfo{}, ErrRejected
		}
		if !seen[check.ecosystem] {
			info.Ecosystems = append(info.Ecosystems, check.ecosystem)
			seen[check.ecosystem] = true
		}
	}
	if seen[EcosystemNode] {
		for _, lock := range []struct{ name, manager string }{{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"}} {
			if _, err := os.Stat(filepath.Join(root, lock.name)); err == nil {
				info.PackageManager = lock.manager
				break
			}
		}
		if info.PackageManager == "" {
			info.PackageManager = "npm"
		}
	}
	if len(info.Ecosystems) == 0 {
		return RepositoryInfo{}, ErrRejected
	}
	return info, nil
}

func BuildPlan(ctx context.Context, info RepositoryInfo, repositoryID, snapshot, environmentID, policyVersion string, target Target) (VerificationPlan, error) {
	if ctx == nil || repositoryID == "" || snapshot == "" || environmentID == "" || policyVersion == "" || info.Root == "" || len(info.Ecosystems) == 0 {
		return VerificationPlan{}, ErrRejected
	}
	if target.Name != "" && !validTarget(target.Name) {
		return VerificationPlan{}, ErrRejected
	}
	commands := make([]Command, 0, len(info.Ecosystems))
	for _, ecosystem := range info.Ecosystems {
		if err := ctx.Err(); err != nil {
			return VerificationPlan{}, err
		}
		command, err := commandFor(info, ecosystem, target)
		if err != nil {
			return VerificationPlan{}, err
		}
		commands = append(commands, command)
	}
	plan := VerificationPlan{RepositoryID: repositoryID, CandidateSnapshot: snapshot, EnvironmentID: environmentID, PolicyVersion: policyVersion, Target: target, Commands: commands}
	plan.PlanDigest = digestPlan(plan)
	return plan, nil
}

func Verify(ctx context.Context, plan VerificationPlan, snapshots SnapshotProvider, runner Runner) (VerificationResult, error) {
	if ctx == nil || snapshots == nil || runner == nil || !validPlan(plan) {
		return VerificationResult{}, ErrRejected
	}
	before, err := snapshots(ctx)
	if err != nil || before != plan.CandidateSnapshot {
		return VerificationResult{}, ErrRejected
	}
	result := VerificationResult{PlanDigest: plan.PlanDigest, RepositoryID: plan.RepositoryID, CandidateSnapshot: plan.CandidateSnapshot, EnvironmentID: plan.EnvironmentID, PolicyVersion: plan.PolicyVersion, Passed: true, VerifiedAt: time.Now().UTC()}
	for _, command := range plan.Commands {
		exitCode, diagnostic := runner.Run(ctx, command)
		if exitCode != 0 {
			result.Passed = false
			result.ExitCode = exitCode
			result.Diagnostic = redact(diagnostic)
			break
		}
	}
	after, err := snapshots(ctx)
	if err != nil || after != plan.CandidateSnapshot {
		return VerificationResult{}, ErrRejected
	}
	return result, nil
}

func ValidResult(result VerificationResult, plan VerificationPlan, currentSnapshot, environmentID, policyVersion string) bool {
	return result.Passed && result.PlanDigest == plan.PlanDigest && result.RepositoryID == plan.RepositoryID && result.CandidateSnapshot == currentSnapshot && result.EnvironmentID == environmentID && result.PolicyVersion == policyVersion && result.VerifiedAt.After(time.Time{}) && result.PlanDigest != ""
}

func commandFor(info RepositoryInfo, ecosystem Ecosystem, target Target) (Command, error) {
	workdir := info.Root
	name := target.Name
	switch ecosystem {
	case EcosystemGo:
		args := []string{"test", "./..."}
		if name != "" {
			args = []string{"test", name}
		}
		return Command{ecosystem, "go", args, workdir}, nil
	case EcosystemNode:
		args := []string{"test"}
		if info.PackageManager == "pnpm" {
			return Command{ecosystem, "pnpm", args, workdir}, nil
		}
		if info.PackageManager == "yarn" {
			return Command{ecosystem, "yarn", args, workdir}, nil
		}
		return Command{ecosystem, "npm", append(args, "--", "--if-present"), workdir}, nil
	case EcosystemRust:
		args := []string{"test"}
		if name != "" {
			args = append(args, name)
		}
		return Command{ecosystem, "cargo", args, workdir}, nil
	case EcosystemNx:
		if target.Affected {
			return Command{ecosystem, "nx", []string{"affected", "-t", "test"}, workdir}, nil
		}
		return Command{ecosystem, "nx", []string{"run-many", "-t", "test"}, workdir}, nil
	case EcosystemTurbo:
		args := []string{"run", "test"}
		if target.Affected {
			args = append(args, "--filter=...[HEAD^]")
		}
		return Command{ecosystem, "turbo", args, workdir}, nil
	case EcosystemBazel:
		return Command{ecosystem, "bazel", []string{"test", "//..."}, workdir}, nil
	default:
		return Command{}, ErrRejected
	}
}

func validPlan(plan VerificationPlan) bool {
	if plan.RepositoryID == "" || plan.CandidateSnapshot == "" || plan.EnvironmentID == "" || plan.PolicyVersion == "" || plan.PlanDigest == "" || len(plan.Commands) == 0 {
		return false
	}
	return digestPlan(plan) == plan.PlanDigest
}

func digestPlan(plan VerificationPlan) string {
	plan.PlanDigest = ""
	data, _ := json.Marshal(plan)
	digest := sha256Bytes(data)
	return digest
}
func sha256Bytes(data []byte) string {
	var sum [32]byte
	sum = sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validTarget(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\x00\r\n") && !strings.Contains(value, "..")
}
func redact(value string) string {
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization: bearer ", "-----begin private key-----", "ghp_", "github_pat_"} {
		if strings.Contains(lower, marker) {
			return "[redacted]"
		}
	}
	if len(value) > 4096 {
		return value[:4096]
	}
	return value
}
func sortedEcosystems(values []Ecosystem) []Ecosystem {
	result := append([]Ecosystem(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
