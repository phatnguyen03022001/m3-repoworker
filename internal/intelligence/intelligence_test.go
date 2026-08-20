package intelligence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectsAllSupportedEcosystems(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"go.mod", "package.json", "Cargo.toml", "nx.json", "turbo.json", "MODULE.bazel"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("manifest\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("lockfile\n"), 0o600); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
	info, err := Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(info.Ecosystems) != 6 || info.PackageManager != "pnpm" {
		t.Fatalf("info = %#v", info)
	}
	if _, err := Detect(context.Background(), t.TempDir()); !errors.Is(err, ErrRejected) {
		t.Fatalf("empty Detect() error = %v", err)
	}
}

func TestNativeVerificationPlansAndTargetBinding(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	info, err := Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	plan, err := BuildPlan(context.Background(), info, "repo", "snapshot-a", "environment-a", "policy-a", Target{Affected: true})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if len(plan.Commands) != 2 || plan.PlanDigest == "" {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Commands[0].Executable != "go" || plan.Commands[1].Executable != "npm" {
		t.Fatalf("commands = %#v", plan.Commands)
	}
	current := "snapshot-a"
	runner := fakeRunner{}
	result, err := Verify(context.Background(), plan, func(context.Context) (string, error) { return current, nil }, runner)
	if err != nil || !result.Passed || !ValidResult(result, plan, current, "environment-a", "policy-a") {
		t.Fatalf("Verify() = %#v, %v", result, err)
	}
	current = "snapshot-b"
	if ValidResult(result, plan, current, "environment-a", "policy-a") {
		t.Fatal("stale result remained valid")
	}
}

func TestVerificationInvalidatesTOCTOUAndSanitizesFailure(t *testing.T) {
	info := RepositoryInfo{Root: "/repo", Ecosystems: []Ecosystem{EcosystemGo}}
	plan, err := BuildPlan(context.Background(), info, "repo", "snapshot-a", "env", "policy", Target{})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	reads := 0
	_, err = Verify(context.Background(), plan, func(context.Context) (string, error) {
		reads++
		if reads == 1 {
			return "snapshot-a", nil
		}
		return "snapshot-b", nil
	}, fakeRunner{})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("TOCTOU Verify() error = %v", err)
	}
	failed, err := Verify(context.Background(), plan, func(context.Context) (string, error) { return "snapshot-a", nil }, fakeRunner{exit: 7, diagnostic: "Authorization: Bearer hidden"})
	if err != nil || failed.Passed || failed.ExitCode != 7 || strings.Contains(failed.Diagnostic, "hidden") {
		t.Fatalf("failed Verify() = %#v, %v", failed, err)
	}
}

func TestInvalidTargetAndPlanAreRejected(t *testing.T) {
	info := RepositoryInfo{Root: "/repo", Ecosystems: []Ecosystem{EcosystemGo}}
	if _, err := BuildPlan(context.Background(), info, "repo", "snapshot", "env", "policy", Target{Name: "../escape"}); !errors.Is(err, ErrRejected) {
		t.Fatalf("invalid target error = %v", err)
	}
	plan := VerificationPlan{RepositoryID: "repo", CandidateSnapshot: "snapshot", EnvironmentID: "env", PolicyVersion: "policy", Commands: []Command{{Executable: "go"}}, PlanDigest: "bad"}
	if ValidResult(VerificationResult{PlanDigest: "bad"}, plan, "snapshot", "env", "policy") {
		t.Fatal("invalid plan result accepted")
	}
}

type fakeRunner struct {
	exit       int
	diagnostic string
}

func (f fakeRunner) Run(context.Context, Command) (int, string) { return f.exit, f.diagnostic }
