// Package runtime owns isolated runtime lifecycle. Apple container is the
// primary adapter; Lima is retained as an explicit fallback/test adapter.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
	"github.com/tienphat/m3-repoworker/internal/workspace"
)

var (
	ErrRejected    = errors.New("runtime request rejected")
	ErrUnsupported = errors.New("runtime backend unavailable")
)

type State string

const (
	StateCreating    State = "CREATING"
	StateReady       State = "READY"
	StateRunning     State = "RUNNING"
	StateStopping    State = "STOPPING"
	StateStopped     State = "STOPPED"
	StateFailed      State = "FAILED"
	StateQuarantined State = "QUARANTINED"
)

type RuntimeSpec struct {
	TaskID             string
	Generation         workspace.Generation
	Lease              workspace.Lease
	WorkspacePath      string
	LiveRepositoryPath string
	Image              string
	CPU                int
	MemoryBytes        int64
	Network            security.NetworkMode
	MountReadOnly      bool
}

type Runtime struct {
	ID              string    `json:"id"`
	Backend         string    `json:"backend"`
	ExternalID      string    `json:"external_id"`
	TaskID          string    `json:"task_id"`
	GenerationID    string    `json:"generation_id"`
	LeaseGeneration uint64    `json:"lease_generation"`
	WorkspacePath   string    `json:"workspace_path"`
	Identity        string    `json:"identity"`
	State           State     `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Adapter interface {
	Name() string
	Create(context.Context, RuntimeSpec, string) (string, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
}

type Manager struct {
	repository *workspace.Repository
	stateRoot  string
	adapters   map[string]Adapter
	runtimes   map[string]Runtime
	mu         sync.Mutex
}

func NewManager(repository *workspace.Repository, stateRoot string, adapters ...Adapter) (*Manager, error) {
	if repository == nil || stateRoot == "" || !filepath.IsAbs(stateRoot) {
		return nil, ErrRejected
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil || os.Chmod(stateRoot, 0o700) != nil {
		return nil, ErrRejected
	}
	manager := &Manager{repository: repository, stateRoot: stateRoot, adapters: map[string]Adapter{}, runtimes: map[string]Runtime{}}
	for _, adapter := range adapters {
		if adapter == nil || adapter.Name() == "" {
			return nil, ErrRejected
		}
		manager.adapters[adapter.Name()] = adapter
	}
	if err := manager.load(); err != nil {
		return nil, ErrRejected
	}
	return manager, nil
}

func (m *Manager) Create(ctx context.Context, spec RuntimeSpec, backend string) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil || m.repository == nil || !validBackend(backend) {
		return Runtime{}, ErrRejected
	}
	adapter, ok := m.adapters[backend]
	if !ok {
		return Runtime{}, ErrUnsupported
	}
	if err := m.validateSpec(ctx, spec); err != nil {
		return Runtime{}, err
	}
	if _, exists := m.runtimes[spec.Generation.ID]; exists {
		return Runtime{}, ErrRejected
	}
	id := runtimeID(spec, backend)
	now := time.Now().UTC()
	runtime := Runtime{ID: id, Backend: backend, TaskID: spec.TaskID, GenerationID: spec.Generation.ID, LeaseGeneration: spec.Lease.FencingGeneration, WorkspacePath: spec.WorkspacePath, Identity: id, State: StateCreating, CreatedAt: now, UpdatedAt: now}
	m.runtimes[spec.Generation.ID] = runtime
	if err := m.persist(runtime); err != nil {
		delete(m.runtimes, spec.Generation.ID)
		return Runtime{}, ErrRejected
	}
	externalID, err := adapter.Create(ctx, spec, id)
	if err != nil {
		runtime.State = StateFailed
		runtime.UpdatedAt = time.Now().UTC()
		_ = m.persist(runtime)
		return Runtime{}, ErrRejected
	}
	runtime.ExternalID = externalID
	runtime.State = StateReady
	runtime.UpdatedAt = time.Now().UTC()
	if err := m.persist(runtime); err != nil {
		return Runtime{}, ErrRejected
	}
	m.runtimes[spec.Generation.ID] = runtime
	return runtime, nil
}

func (m *Manager) Start(ctx context.Context, generationID string, lease workspace.Lease) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime, adapter, err := m.runtimeFor(generationID, StateReady)
	if err != nil {
		return Runtime{}, err
	}
	if lease.GenerationID != runtime.GenerationID || lease.FencingGeneration != runtime.LeaseGeneration {
		return Runtime{}, ErrRejected
	}
	if err := m.repository.AssertLease(ctx, lease); err != nil {
		return Runtime{}, ErrRejected
	}
	if err := adapter.Start(ctx, runtime.ExternalID); err != nil {
		runtime.State = StateFailed
		runtime.UpdatedAt = time.Now().UTC()
		_ = m.persist(runtime)
		return Runtime{}, ErrRejected
	}
	runtime.State = StateRunning
	runtime.UpdatedAt = time.Now().UTC()
	m.runtimes[generationID] = runtime
	if err := m.persist(runtime); err != nil {
		return Runtime{}, ErrRejected
	}
	return runtime, nil
}

func (m *Manager) Stop(ctx context.Context, generationID string, lease workspace.Lease) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime, adapter, err := m.runtimeFor(generationID, StateRunning)
	if err != nil {
		return Runtime{}, err
	}
	if lease.GenerationID != runtime.GenerationID || lease.FencingGeneration != runtime.LeaseGeneration || m.repository.AssertLease(ctx, lease) != nil {
		return Runtime{}, ErrRejected
	}
	runtime.State = StateStopping
	runtime.UpdatedAt = time.Now().UTC()
	if err := m.persist(runtime); err != nil {
		return Runtime{}, ErrRejected
	}
	if err := adapter.Stop(ctx, runtime.ExternalID); err != nil {
		runtime.State = StateQuarantined
		runtime.UpdatedAt = time.Now().UTC()
		_ = m.persist(runtime)
		return Runtime{}, ErrRejected
	}
	runtime.State = StateStopped
	runtime.UpdatedAt = time.Now().UTC()
	m.runtimes[generationID] = runtime
	if err := m.persist(runtime); err != nil {
		return Runtime{}, ErrRejected
	}
	return runtime, nil
}

func (m *Manager) Delete(ctx context.Context, generationID string, lease workspace.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	runtime, adapter, err := m.runtimeFor(generationID, StateStopped)
	if err != nil {
		return err
	}
	if lease.GenerationID != runtime.GenerationID || lease.FencingGeneration != runtime.LeaseGeneration || m.repository.AssertLease(ctx, lease) != nil {
		return ErrRejected
	}
	if err := adapter.Delete(ctx, runtime.ExternalID); err != nil {
		return ErrRejected
	}
	delete(m.runtimes, generationID)
	return os.Remove(m.runtimePath(runtime.ID))
}

// Status returns the persisted runtime record for one generation without
// changing lifecycle state. Callers still need a valid lease before using the
// runtime for execution.
func (m *Manager) Status(ctx context.Context, generationID string) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil || m == nil || generationID == "" {
		return Runtime{}, ErrRejected
	}
	runtime, ok := m.runtimes[generationID]
	if !ok {
		return Runtime{}, ErrRejected
	}
	return runtime, nil
}

// Lookup resolves an opaque runtime ID to its persisted record. It is used by
// the process starter; callers still perform lease and lifecycle checks before
// execution.
func (m *Manager) Lookup(ctx context.Context, runtimeID string) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil || m == nil || runtimeID == "" {
		return Runtime{}, ErrRejected
	}
	for _, runtime := range m.runtimes {
		if runtime.ID == runtimeID {
			return runtime, nil
		}
	}
	return Runtime{}, ErrRejected
}

// Recover cleans up persisted runtimes left in an active lifecycle state after
// a manager crash. Failed cleanup is quarantined for explicit inspection.
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for generationID, runtime := range m.runtimes {
		if runtime.State != StateCreating && runtime.State != StateRunning && runtime.State != StateStopping {
			continue
		}
		adapter, ok := m.adapters[runtime.Backend]
		if !ok || adapter.Stop(ctx, runtime.ExternalID) != nil || adapter.Delete(ctx, runtime.ExternalID) != nil {
			runtime.State = StateQuarantined
		} else {
			runtime.State = StateStopped
		}
		runtime.UpdatedAt = time.Now().UTC()
		m.runtimes[generationID] = runtime
		if err := m.persist(runtime); err != nil {
			return ErrRejected
		}
	}
	return nil
}

func (m *Manager) validateSpec(ctx context.Context, spec RuntimeSpec) error {
	if spec.TaskID == "" || strings.ContainsAny(spec.TaskID, "/\\\x00\r\n") || spec.Generation.ID == "" || spec.WorkspacePath == "" || !filepath.IsAbs(spec.WorkspacePath) || filepath.Clean(spec.WorkspacePath) != spec.WorkspacePath || !filepath.IsAbs(spec.LiveRepositoryPath) || filepath.Clean(spec.LiveRepositoryPath) != spec.LiveRepositoryPath || spec.Image == "" || strings.ContainsAny(spec.Image, "\x00\r\n") || spec.CPU <= 0 || spec.CPU > 256 || spec.MemoryBytes < 64<<20 || spec.MemoryBytes > 1<<40 {
		return ErrRejected
	}
	if spec.Network == security.NetworkFull {
		return ErrRejected
	}
	if sameOrWithin(spec.LiveRepositoryPath, spec.WorkspacePath) || sameOrWithin(spec.WorkspacePath, spec.LiveRepositoryPath) {
		return ErrRejected
	}
	if spec.WorkspacePath != spec.Generation.Path {
		return ErrRejected
	}
	if err := m.repository.AssertGeneration(ctx, spec.Generation, spec.Lease); err != nil {
		return err
	}
	return nil
}

func (m *Manager) runtimeFor(generationID string, expected State) (Runtime, Adapter, error) {
	runtime, ok := m.runtimes[generationID]
	if !ok || runtime.State != expected {
		return Runtime{}, nil, ErrRejected
	}
	adapter, ok := m.adapters[runtime.Backend]
	if !ok {
		return Runtime{}, nil, ErrUnsupported
	}
	return runtime, adapter, nil
}

func (m *Manager) load() error {
	entries, err := os.ReadDir(m.stateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".runtime.json") {
			continue
		}
		file, err := os.Open(filepath.Join(m.stateRoot, entry.Name()))
		if err != nil {
			return err
		}
		var runtime Runtime
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&runtime)
		_ = file.Close()
		if err != nil || !validRuntime(runtime) {
			return ErrRejected
		}
		m.runtimes[runtime.GenerationID] = runtime
	}
	return nil
}

func (m *Manager) persist(runtime Runtime) error {
	if !validRuntime(runtime) {
		return ErrRejected
	}
	data, err := json.Marshal(runtime)
	if err != nil {
		return err
	}
	path := m.runtimePath(runtime.ID)
	temporary, err := os.CreateTemp(m.stateRoot, ".runtime-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
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
	return os.Rename(temporaryPath, path)
}

func (m *Manager) runtimePath(id string) string {
	return filepath.Join(m.stateRoot, id+".runtime.json")
}

func validBackend(value string) bool {
	return value == "apple-container" || value == "lima" || value == "test"
}

func validRuntime(runtime Runtime) bool {
	return runtime.ID != "" && validBackend(runtime.Backend) && (runtime.ExternalID != "" || runtime.State == StateCreating) && runtime.TaskID != "" && runtime.GenerationID != "" && runtime.LeaseGeneration > 0 && filepath.IsAbs(runtime.WorkspacePath) && runtime.Identity == runtime.ID && runtime.State != ""
}

func runtimeID(spec RuntimeSpec, backend string) string {
	data := fmt.Sprintf("%s:%s:%d:%s:%s", spec.TaskID, spec.Generation.ID, spec.Lease.FencingGeneration, spec.WorkspacePath, backend)
	digest := sha256.Sum256([]byte(data))
	return "runtime_" + hex.EncodeToString(digest[:16])
}

func sameOrWithin(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/opt/homebrew/bin", "LC_ALL=C"}
	return command.CombinedOutput()
}

type AppleContainerAdapter struct {
	Runner CommandRunner
	Binary string
}

func (a AppleContainerAdapter) Name() string { return "apple-container" }

func (a AppleContainerAdapter) runner() CommandRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return execRunner{}
}

func (a AppleContainerAdapter) binary() string {
	if a.Binary != "" {
		return a.Binary
	}
	return "container"
}

func (a AppleContainerAdapter) Create(ctx context.Context, spec RuntimeSpec, identity string) (string, error) {
	if spec.Network == security.NetworkFull || spec.Network == security.NetworkRegistry {
		return "", ErrUnsupported
	}
	args := []string{"create", "--name", identity, "--cpus", strconv.Itoa(spec.CPU), "--memory", strconv.FormatInt(spec.MemoryBytes, 10), "--network", "none"}
	mount := "source=" + spec.WorkspacePath + ",target=/workspace"
	if spec.MountReadOnly {
		mount += ",readonly"
	}
	args = append(args, "--mount", mount, spec.Image, "/bin/sleep", "infinity")
	if _, err := a.runner().Run(ctx, a.binary(), args...); err != nil {
		return "", ErrUnsupported
	}
	return identity, nil
}

func (a AppleContainerAdapter) Start(ctx context.Context, id string) error {
	_, err := a.runner().Run(ctx, a.binary(), "start", id)
	if err != nil {
		return ErrUnsupported
	}
	return nil
}
func (a AppleContainerAdapter) Stop(ctx context.Context, id string) error {
	_, err := a.runner().Run(ctx, a.binary(), "stop", id)
	if err != nil {
		return ErrUnsupported
	}
	return nil
}
func (a AppleContainerAdapter) Delete(ctx context.Context, id string) error {
	_, err := a.runner().Run(ctx, a.binary(), "rm", id)
	if err != nil {
		return ErrUnsupported
	}
	return nil
}

type LimaAdapter struct {
	Runner   CommandRunner
	Instance string
}

func (a LimaAdapter) Name() string { return "lima" }
func (a LimaAdapter) instance() string {
	if a.Instance != "" {
		return a.Instance
	}
	return "default"
}
func (a LimaAdapter) runner() CommandRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return execRunner{}
}
func (a LimaAdapter) Create(ctx context.Context, spec RuntimeSpec, identity string) (string, error) {
	if spec.Network == security.NetworkFull || spec.Network == security.NetworkRegistry {
		return "", ErrUnsupported
	}
	if _, err := a.runner().Run(ctx, "limactl", "shell", "--start", a.instance(), "--", "true"); err != nil {
		return "", ErrUnsupported
	}
	return "lima:" + a.instance() + ":" + identity, nil
}
func (a LimaAdapter) Start(context.Context, string) error  { return nil }
func (a LimaAdapter) Stop(context.Context, string) error   { return nil }
func (a LimaAdapter) Delete(context.Context, string) error { return nil }

type FakeAdapter struct {
	BackendName string
	Calls       []string
	Err         error
	mu          sync.Mutex
}

func (f *FakeAdapter) Name() string {
	if f.BackendName == "" {
		return "test"
	}
	return f.BackendName
}
func (f *FakeAdapter) call(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name)
	return f.Err
}
func (f *FakeAdapter) Create(context.Context, RuntimeSpec, string) (string, error) {
	if err := f.call("create"); err != nil {
		return "", err
	}
	return "fake-runtime", nil
}
func (f *FakeAdapter) Start(context.Context, string) error  { return f.call("start") }
func (f *FakeAdapter) Stop(context.Context, string) error   { return f.call("stop") }
func (f *FakeAdapter) Delete(context.Context, string) error { return f.call("delete") }
