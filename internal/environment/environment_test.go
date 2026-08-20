package environment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tienphat/m3-repoworker/internal/security"
)

func TestDetectionAndLockfileIdentityAreDeterministic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/app\n\ngo 1.26.6\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte("sum\n"), 0o600); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}
	toolchain, err := Detect(context.Background(), root)
	if err != nil || toolchain.Kind != ToolchainGo || toolchain.Version != "1.26.6" || toolchain.Identity == "" {
		t.Fatalf("Detect() = %#v, %v", toolchain, err)
	}
	first, err := HashLockfiles(context.Background(), root)
	if err != nil {
		t.Fatalf("HashLockfiles(first) error = %v", err)
	}
	second, err := HashLockfiles(context.Background(), root)
	if err != nil || first != second {
		t.Fatalf("HashLockfiles() = %q, %q, %v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("change go.sum: %v", err)
	}
	third, err := HashLockfiles(context.Background(), root)
	if err != nil || third == first {
		t.Fatalf("lockfile hash did not change: %q %q %v", first, third, err)
	}
}

func TestEnvironmentGenerationAndRegistryOnlyInstall(t *testing.T) {
	root := t.TempDir()
	cache, err := NewCache(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	installer := &FakeInstaller{}
	manager, err := NewManager(filepath.Join(root, "environments"), cache, installer)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	spec := EnvironmentSpec{RepositoryID: strings.Repeat("a", 64), WorkspaceID: "gen_1", Platform: "darwin/arm64", Toolchain: Toolchain{Kind: ToolchainGo, Version: "1.26.6", Identity: strings.Repeat("b", 64)}, LockfileDigest: strings.Repeat("c", 64), PolicyVersion: "m3.2-v1", Image: "go:1.26"}
	generation, err := manager.Create(context.Background(), spec)
	if err != nil || generation.Identity == "" || generation.State != "READY" {
		t.Fatalf("Create() = %#v, %v", generation, err)
	}
	policy := security.CompiledPolicy{Network: security.NetworkPolicy{Mode: security.NetworkRegistry, RegistryDomain: []string{"registry.example"}}}
	if err := manager.Install(context.Background(), generation, []string{"example.invalid/module@v1.0.0"}, policy); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if len(installer.Calls) != 1 || !strings.Contains(installer.Calls[0], "registry.example") {
		t.Fatalf("installer calls = %#v", installer.Calls)
	}
	policy.Network.Mode = security.NetworkNone
	if err := manager.Install(context.Background(), generation, []string{"package"}, policy); !errors.Is(err, ErrRejected) {
		t.Fatalf("non-registry Install() error = %v", err)
	}
}

func TestEnvironmentGenerationRehydratesWithStableIdentity(t *testing.T) {
	root := t.TempDir()
	cache, err := NewCache(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	spec := EnvironmentSpec{RepositoryID: strings.Repeat("a", 64), WorkspaceID: "gen_1", Platform: "darwin/arm64", Toolchain: Toolchain{Kind: ToolchainGo, Version: "1.26.6", Identity: strings.Repeat("b", 64)}, LockfileDigest: strings.Repeat("c", 64), PolicyVersion: "m3.2-v1", Image: "go:1.26"}
	first, err := NewManager(filepath.Join(root, "environments"), cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := first.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(filepath.Join(root, "environments"), cache, nil)
	if err != nil {
		t.Fatalf("NewManager(restart) error = %v", err)
	}
	rehydrated, err := restarted.Create(context.Background(), spec)
	if err != nil || rehydrated != generation {
		t.Fatalf("Create(restart) = %#v, %v; want %#v", rehydrated, err, generation)
	}
	changed := spec
	changed.Image = "go:1.27"
	changedGeneration, err := restarted.Create(context.Background(), changed)
	if err != nil || changedGeneration.Identity == generation.Identity {
		t.Fatalf("Create(changed) = %#v, %v; want a new binding", changedGeneration, err)
	}
}

func TestCacheColdWarmEquivalenceAndPoisoningRejection(t *testing.T) {
	root := t.TempDir()
	cache, err := NewCache(root)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	source := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(source, []byte("verified artifact\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	binding := CacheBinding{ToolchainIdentity: strings.Repeat("a", 64), Platform: "darwin/arm64", LockfileDigest: strings.Repeat("b", 64), PolicyVersion: "m3.2-v1"}
	key, err := CacheKey(binding, "compile-output")
	if err != nil {
		t.Fatalf("CacheKey() error = %v", err)
	}
	if _, err := cache.Get(context.Background(), key, binding); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("cold Get() error = %v", err)
	}
	artifact, err := cache.Put(context.Background(), key, binding, source)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	warm, err := cache.Get(context.Background(), key, binding)
	if err != nil || warm.ContentDigest != artifact.ContentDigest {
		t.Fatalf("warm Get() = %#v, %v", warm, err)
	}
	wrong := binding
	wrong.PolicyVersion = "m3.2-v2"
	if _, err := cache.Get(context.Background(), key, wrong); !errors.Is(err, ErrRejected) {
		t.Fatalf("poisoned binding Get() error = %v", err)
	}
	if err := os.WriteFile(artifact.Path, []byte("poisoned\n"), 0o600); err != nil {
		t.Fatalf("poison artifact: %v", err)
	}
	if _, err := cache.Get(context.Background(), key, binding); !errors.Is(err, ErrRejected) {
		t.Fatalf("corrupt artifact Get() error = %v", err)
	}
	if err := cache.DeleteAll(); err != nil {
		t.Fatalf("DeleteAll() error = %v", err)
	}
	if _, err := cache.Get(context.Background(), key, binding); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("post-delete Get() error = %v", err)
	}
}

func TestDetectionRejectsSymlinkManifest(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module outside\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "go.mod"), filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Detect(context.Background(), root); !errors.Is(err, ErrRejected) {
		t.Fatalf("symlink Detect() error = %v", err)
	}
}
