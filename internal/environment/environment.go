// Package environment provides deterministic environment identities and a
// verified cache. Cache hits accelerate work but never determine correctness.
package environment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
)

var (
	ErrRejected  = errors.New("environment request rejected")
	ErrCacheMiss = errors.New("environment cache miss")
)

type ToolchainKind string

const (
	ToolchainGo    ToolchainKind = "go"
	ToolchainNode  ToolchainKind = "node"
	ToolchainRust  ToolchainKind = "rust"
	ToolchainNx    ToolchainKind = "nx"
	ToolchainTurbo ToolchainKind = "turbo"
	ToolchainBazel ToolchainKind = "bazel"
)

type Toolchain struct {
	Kind     ToolchainKind `json:"kind"`
	Version  string        `json:"version"`
	Manifest string        `json:"manifest"`
	Identity string        `json:"identity"`
}

type EnvironmentSpec struct {
	RepositoryID   string    `json:"repository_id"`
	WorkspaceID    string    `json:"workspace_id"`
	Platform       string    `json:"platform"`
	Toolchain      Toolchain `json:"toolchain"`
	LockfileDigest string    `json:"lockfile_digest"`
	PolicyVersion  string    `json:"policy_version"`
	Image          string    `json:"image"`
}

type Generation struct {
	ID        string          `json:"id"`
	Identity  string          `json:"identity"`
	Spec      EnvironmentSpec `json:"spec"`
	State     string          `json:"state"`
	CreatedAt time.Time       `json:"created_at"`
}

type CacheBinding struct {
	ToolchainIdentity string `json:"toolchain_identity"`
	Platform          string `json:"platform"`
	LockfileDigest    string `json:"lockfile_digest"`
	PolicyVersion     string `json:"policy_version"`
}

type Artifact struct {
	Key           string       `json:"key"`
	Binding       CacheBinding `json:"binding"`
	ContentDigest string       `json:"content_digest"`
	Path          string       `json:"path"`
	Size          int64        `json:"size"`
	CreatedAt     time.Time    `json:"created_at"`
}

type Detector struct{}

type Installer interface {
	Install(context.Context, string, []string, []string) error
}

type Manager struct {
	root      string
	cache     *Cache
	installer Installer
	mu        sync.Mutex
	gens      map[string]Generation
}

func Detect(ctx context.Context, root string) (Toolchain, error) {
	if ctx == nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Toolchain{}, ErrRejected
	}
	checks := []struct {
		name string
		kind ToolchainKind
	}{
		{"go.mod", ToolchainGo}, {"package.json", ToolchainNode}, {"Cargo.toml", ToolchainRust},
		{"nx.json", ToolchainNx}, {"turbo.json", ToolchainTurbo}, {"MODULE.bazel", ToolchainBazel}, {"WORKSPACE", ToolchainBazel},
	}
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			return Toolchain{}, err
		}
		path := filepath.Join(root, check.name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<20 {
			return Toolchain{}, ErrRejected
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Toolchain{}, ErrRejected
		}
		version := detectVersion(check.kind, data)
		manifest := digestBytes(data)
		identity := digestString(string(check.kind) + "\x00" + version + "\x00" + manifest)
		return Toolchain{Kind: check.kind, Version: version, Manifest: manifest, Identity: identity}, nil
	}
	return Toolchain{}, ErrRejected
}

func HashLockfiles(ctx context.Context, root string) (string, error) {
	if ctx == nil || root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrRejected
	}
	known := []string{"go.mod", "go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "Cargo.lock", "rust-toolchain.toml", "MODULE.bazel.lock", "nx.json", "turbo.json", "WORKSPACE.bazel"}
	var present []string
	for _, name := range known {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16<<20 {
			return "", ErrRejected
		}
		present = append(present, name)
	}
	sort.Strings(present)
	hash := sha256.New()
	for _, name := range present {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return "", ErrRejected
		}
		_, _ = io.WriteString(hash, name+"\x00")
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func NewManager(root string, cache *Cache, installer Installer) (*Manager, error) {
	if root == "" || !filepath.IsAbs(root) || cache == nil {
		return nil, ErrRejected
	}
	if err := os.MkdirAll(root, 0o700); err != nil || os.Chmod(root, 0o700) != nil {
		return nil, ErrRejected
	}
	manager := &Manager{root: root, cache: cache, installer: installer, gens: map[string]Generation{}}
	if err := manager.load(); err != nil {
		return nil, ErrRejected
	}
	return manager, nil
}

func (m *Manager) Create(ctx context.Context, spec EnvironmentSpec) (Generation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil || !validSpec(spec) {
		return Generation{}, ErrRejected
	}
	identity := environmentIdentity(spec)
	id := "env_" + identity[:32]
	if existing, exists := m.gens[id]; exists {
		if existing.Identity == identity && existing.Spec == spec && existing.State == "READY" {
			return existing, nil
		}
		return Generation{}, ErrRejected
	}
	generation := Generation{ID: id, Identity: identity, Spec: spec, State: "READY", CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(m.root, id+".json"), generation); err != nil {
		return Generation{}, ErrRejected
	}
	m.gens[id] = generation
	return generation, nil
}

func (m *Manager) load() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file, err := os.Open(filepath.Join(m.root, entry.Name()))
		if err != nil {
			return err
		}
		var generation Generation
		decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&generation)
		_ = file.Close()
		if err != nil || !validGeneration(generation) || entry.Name() != generation.ID+".json" || generation.Identity != environmentIdentity(generation.Spec) {
			return ErrRejected
		}
		m.gens[generation.ID] = generation
	}
	return nil
}

func validGeneration(generation Generation) bool {
	return validOpaque(generation.ID) && strings.HasPrefix(generation.ID, "env_") && validIdentity(generation.Identity) && validSpec(generation.Spec) && generation.State == "READY" && !generation.CreatedAt.IsZero()
}

func (m *Manager) Install(ctx context.Context, generation Generation, packages []string, policy security.CompiledPolicy) error {
	if ctx == nil || m.installer == nil || generation.State != "READY" || !validSpec(generation.Spec) || policy.Network.Mode != security.NetworkRegistry || len(policy.Network.RegistryDomain) == 0 || len(packages) == 0 {
		return ErrRejected
	}
	for _, packageName := range packages {
		if packageName == "" || strings.ContainsAny(packageName, "\x00\r\n") {
			return ErrRejected
		}
	}
	return m.installer.Install(ctx, generation.Identity, append([]string(nil), packages...), append([]string(nil), policy.Network.RegistryDomain...))
}

func environmentIdentity(spec EnvironmentSpec) string {
	data, _ := json.Marshal(spec)
	return digestBytes(data)
}

func validSpec(spec EnvironmentSpec) bool {
	return validIdentity(spec.RepositoryID) && validOpaque(spec.WorkspaceID) && spec.Platform != "" && spec.Toolchain.Identity != "" && validIdentity(spec.LockfileDigest) && spec.PolicyVersion != "" && spec.Image != "" && !strings.ContainsAny(spec.Image, "\x00\r\n")
}

func detectVersion(kind ToolchainKind, data []byte) string {
	text := string(data)
	if kind == ToolchainGo {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "go" {
				return fields[1]
			}
		}
	}
	if kind == ToolchainNode && strings.Contains(text, "packageManager") {
		return "package-manager-declared"
	}
	if kind == ToolchainRust && strings.Contains(text, "channel") {
		return "toolchain-declared"
	}
	return "manifest-detected"
}

type Cache struct {
	root string
	mu   sync.Mutex
}

func NewCache(root string) (*Cache, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, ErrRejected
	}
	if err := os.MkdirAll(root, 0o700); err != nil || os.Chmod(root, 0o700) != nil {
		return nil, ErrRejected
	}
	return &Cache{root: root}, nil
}

func CacheKey(binding CacheBinding, artifactName string) (string, error) {
	if !validBinding(binding) || artifactName == "" || strings.ContainsAny(artifactName, "\x00\r\n") {
		return "", ErrRejected
	}
	data, err := json.Marshal(struct {
		Binding CacheBinding `json:"binding"`
		Name    string       `json:"name"`
	}{binding, artifactName})
	if err != nil {
		return "", ErrRejected
	}
	return digestBytes(data), nil
}

func (c *Cache) Put(ctx context.Context, key string, binding CacheBinding, source string) (Artifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx == nil || c == nil || !validIdentity(key) || !validBinding(binding) || source == "" || !filepath.IsAbs(source) {
		return Artifact{}, ErrRejected
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 1<<30 {
		return Artifact{}, ErrRejected
	}
	digest, size, err := fileDigest(source)
	if err != nil {
		return Artifact{}, ErrRejected
	}
	artifactPath := filepath.Join(c.root, key+"-"+digest)
	if err := copyAtomic(source, artifactPath, 0o600); err != nil {
		return Artifact{}, ErrRejected
	}
	artifact := Artifact{Key: key, Binding: binding, ContentDigest: digest, Path: artifactPath, Size: size, CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(c.root, key+".json"), artifact); err != nil {
		return Artifact{}, ErrRejected
	}
	return artifact, nil
}

func (c *Cache) Get(ctx context.Context, key string, binding CacheBinding) (Artifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx == nil || c == nil || !validIdentity(key) || !validBinding(binding) {
		return Artifact{}, ErrRejected
	}
	var artifact Artifact
	if err := readJSON(filepath.Join(c.root, key+".json"), &artifact); err != nil {
		return Artifact{}, ErrCacheMiss
	}
	if artifact.Key != key || artifact.Binding != binding || !filepath.IsAbs(artifact.Path) || !pathWithin(c.root, artifact.Path) {
		return Artifact{}, ErrRejected
	}
	info, err := os.Lstat(artifact.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != artifact.Size {
		return Artifact{}, ErrRejected
	}
	digest, size, err := fileDigest(artifact.Path)
	if err != nil || size != artifact.Size || digest != artifact.ContentDigest {
		return Artifact{}, ErrRejected
	}
	return artifact, nil
}

func (c *Cache) DeleteAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return ErrRejected
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(c.root, entry.Name())); err != nil {
			return ErrRejected
		}
	}
	return nil
}

type FakeInstaller struct {
	Calls []string
	mu    sync.Mutex
}

func (f *FakeInstaller) Install(_ context.Context, identity string, packages, domains []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, identity+":"+strings.Join(packages, ",")+":"+strings.Join(domains, ","))
	return nil
}

func validBinding(binding CacheBinding) bool {
	return validIdentity(binding.ToolchainIdentity) && binding.Platform != "" && validIdentity(binding.LockfileDigest) && binding.PolicyVersion != ""
}
func validIdentity(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func validOpaque(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n/\\")
}
func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func digestString(value string) string { return digestBytes([]byte(value)) }
func pathWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
func copyAtomic(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".cache-")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
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
	return os.Rename(tempPath, destination)
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
