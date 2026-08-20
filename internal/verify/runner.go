package verify

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	InternalPresetEnvironment = "M3_REPOWORKER_INTERNAL_FIXED_PRESET"
	CaptureLimit              = 32 << 10
	DiagnosticLimit           = 12 << 10
	internalRootFD            = 3
	internalControlFD         = 4
)

var ErrRejected = errors.New("fixed command rejected")

type Preset string

const (
	PresetFmt            Preset = "fmt"
	PresetTest           Preset = "test"
	PresetTestRace       Preset = "test-race"
	PresetVet            Preset = "vet"
	PresetMCPIntegration Preset = "mcp-integration"
	PresetVerify         Preset = "verify"
	PresetGoModTidy      Preset = "go-mod-tidy"
)

type FailureStage string

const (
	StageResolve   FailureStage = "resolve"
	StageStart     FailureStage = "start"
	StageExecution FailureStage = "execution"
	StageTimeout   FailureStage = "timeout"
	StageSanitize  FailureStage = "sanitize"
)

type Outcome struct {
	ExitCode     int
	TimedOut     bool
	FailureStage FailureStage
	Diagnostic   string
	Truncated    bool
}

type commandDefinition struct {
	executable string
	arguments  []string
	timeout    time.Duration
	network    bool
}

var commandDefinitions = map[Preset]commandDefinition{
	PresetFmt:            {executable: "make", arguments: []string{"fmt-check"}, timeout: 2 * time.Minute},
	PresetTest:           {executable: "make", arguments: []string{"test"}, timeout: 5 * time.Minute},
	PresetTestRace:       {executable: "make", arguments: []string{"test-race"}, timeout: 10 * time.Minute},
	PresetVet:            {executable: "make", arguments: []string{"vet"}, timeout: 5 * time.Minute},
	PresetMCPIntegration: {executable: "make", arguments: []string{"mcp-integration"}, timeout: 5 * time.Minute},
	PresetVerify:         {executable: "make", arguments: []string{"verify"}, timeout: 15 * time.Minute},
	PresetGoModTidy:      {executable: "go", arguments: []string{"mod", "tidy"}, timeout: 10 * time.Minute, network: true},
}

func VerificationPreset(check string) (Preset, bool) {
	preset := Preset(check)
	switch preset {
	case PresetFmt, PresetTest, PresetTestRace, PresetVet, PresetMCPIntegration, PresetVerify:
		return preset, true
	default:
		return "", false
	}
}

func InternalRequest() (Preset, bool) {
	value, ok := os.LookupEnv(InternalPresetEnvironment)
	return Preset(value), ok && value != ""
}

func Run(ctx context.Context, root *os.File, preset Preset, redactions []string) (Outcome, error) {
	definition, ok := commandDefinitions[preset]
	if !ok {
		return Outcome{}, ErrRejected
	}
	return runWithTimeout(ctx, root, preset, redactions, definition.timeout)
}

func runWithTimeout(ctx context.Context, root *os.File, preset Preset, redactions []string, timeout time.Duration) (Outcome, error) {
	if ctx == nil || root == nil || timeout <= 0 {
		return Outcome{}, ErrRejected
	}
	if _, ok := commandDefinitions[preset]; !ok {
		return Outcome{}, ErrRejected
	}
	info, err := root.Stat()
	if err != nil || !info.IsDir() {
		return Outcome{}, ErrRejected
	}
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return Outcome{ExitCode: -1, FailureStage: StageResolve}, nil
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(executable) {
		return Outcome{ExitCode: -1, FailureStage: StageResolve}, nil
	}

	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		return Outcome{ExitCode: -1, FailureStage: StageStart}, nil
	}
	defer controlReader.Close()

	capture := &boundedCapture{limit: CaptureLimit}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.Command(executable)
	command.ExtraFiles = []*os.File{root, controlWriter}
	command.Env = append(helperEnvironment(), InternalPresetEnvironment+"="+string(preset))
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Stdin = nil
	command.Stdout = capture
	command.Stderr = capture
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		controlWriter.Close()
		return failedOutcome(-1, StageStart, capture, redactions), nil
	}
	controlWriter.Close()

	wait := make(chan error, 1)
	go func() {
		wait <- command.Wait()
	}()

	select {
	case waitErr := <-wait:
		stage := readControlStage(controlReader)
		if waitErr == nil {
			return Outcome{ExitCode: 0}, nil
		}
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if stage == "" {
			stage = StageExecution
		}
		return failedOutcome(exitCode, stage, capture, redactions), nil
	case <-runCtx.Done():
		if command.Process != nil {
			_ = unix.Kill(-command.Process.Pid, unix.SIGKILL)
		}
		<-wait
		_ = readControlStage(controlReader)
		if ctx.Err() != nil {
			return failedOutcome(-1, StageExecution, capture, redactions), ctx.Err()
		}
		outcome := failedOutcome(-1, StageTimeout, capture, redactions)
		outcome.TimedOut = true
		return outcome, nil
	}
}

func failedOutcome(exitCode int, stage FailureStage, capture *boundedCapture, redactions []string) Outcome {
	diagnostic, sanitizedTruncated, suppressed := sanitizeDiagnostic(capture.Bytes(), redactions)
	if suppressed {
		stage = StageSanitize
		diagnostic = ""
	}
	return Outcome{
		ExitCode:     exitCode,
		FailureStage: stage,
		Diagnostic:   diagnostic,
		Truncated:    capture.Truncated() || sanitizedTruncated,
	}
}

func readControlStage(reader io.Reader) FailureStage {
	data, err := io.ReadAll(io.LimitReader(reader, 64))
	if err != nil {
		return StageSanitize
	}
	stage := FailureStage(strings.TrimSpace(string(data)))
	switch stage {
	case "", StageResolve, StageStart:
		return stage
	default:
		return StageSanitize
	}
}

func RunInternal(preset Preset) int {
	control := os.NewFile(internalControlFD, "fixed-runner-control")
	fail := func(stage FailureStage) int {
		if control != nil {
			_, _ = control.WriteString(string(stage))
			_ = control.Close()
		}
		return 125
	}
	definition, ok := commandDefinitions[preset]
	if !ok {
		return fail(StageResolve)
	}
	root := os.NewFile(internalRootFD, "repository-root")
	if root == nil {
		return fail(StageStart)
	}
	var rootStat unix.Stat_t
	if err := unix.Fstat(internalRootFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fail(StageStart)
	}
	if err := unix.Fchdir(internalRootFD); err != nil {
		return fail(StageStart)
	}
	if !currentDirectoryMatches(internalRootFD, rootStat) {
		return fail(StageStart)
	}
	verifiedCWD, err := verifiedWorkingDirectory(internalRootFD, rootStat)
	if err != nil {
		return fail(StageStart)
	}
	cacheRootFD, err := openOrCreateDirectoryAt(internalRootFD, ".cache", uint64(rootStat.Dev))
	if err != nil {
		return fail(StageStart)
	}
	if err := unix.Close(cacheRootFD); err != nil {
		return fail(StageStart)
	}
	cache, err := configuredCacheDirectories()
	if err != nil {
		return fail(StageStart)
	}
	if cache == (cachePaths{}) {
		cache, err = ensureCacheDirectories(internalRootFD, uint64(rootStat.Dev), verifiedCWD)
	}
	if err != nil {
		return fail(StageStart)
	}
	if control != nil {
		unix.CloseOnExec(internalControlFD)
	}

	executable, err := resolveExecutable(definition.executable)
	if err != nil {
		return fail(StageResolve)
	}
	arguments := append([]string{executable}, definition.arguments...)
	commandEnvironmentValues := append(commandEnvironment(preset, cache), InternalPresetEnvironment+"="+string(preset))
	if err := unix.Exec(executable, arguments, commandEnvironmentValues); err != nil {
		return fail(StageStart)
	}
	return 0
}

func resolveExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil || !filepath.IsAbs(path) {
		return "", ErrRejected
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) {
		return "", ErrRejected
	}
	return path, nil
}

func currentDirectoryMatches(rootFD int, expected unix.Stat_t) bool {
	currentFD, err := unix.Open(".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(currentFD)
	var current unix.Stat_t
	if err := unix.Fstat(currentFD, &current); err != nil {
		return false
	}
	return uint64(current.Dev) == uint64(expected.Dev) && uint64(current.Ino) == uint64(expected.Ino)
}

func verifiedWorkingDirectory(rootFD int, expected unix.Stat_t) (string, error) {
	if !currentDirectoryMatches(rootFD, expected) {
		return "", ErrRejected
	}
	cwd, err := os.Getwd()
	if err != nil || !filepath.IsAbs(cwd) {
		return "", ErrRejected
	}
	if err := verifyRootPath(rootFD, cwd, expected); err != nil {
		return "", err
	}
	return filepath.Clean(cwd), nil
}

func verifyRootPath(rootFD int, path string, expected unix.Stat_t) error {
	if !filepath.IsAbs(path) {
		return ErrRejected
	}
	pathFD, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrRejected
	}
	defer unix.Close(pathFD)
	var pathStat unix.Stat_t
	if err := unix.Fstat(pathFD, &pathStat); err != nil {
		return ErrRejected
	}
	var openedStat unix.Stat_t
	if err := unix.Fstat(rootFD, &openedStat); err != nil {
		return ErrRejected
	}
	if uint64(openedStat.Dev) != uint64(expected.Dev) || uint64(openedStat.Ino) != uint64(expected.Ino) ||
		uint64(pathStat.Dev) != uint64(expected.Dev) || uint64(pathStat.Ino) != uint64(expected.Ino) {
		return ErrRejected
	}
	return nil
}

type cachePaths struct {
	goBuild string
	goMod   string
	goPath  string
}

// configuredCacheDirectories honors the fixed cache paths supplied by the
// controlled Makefile/bootstrap environment. It rejects symlinked roots and
// creates private directories before the fixed preset command is executed.
func configuredCacheDirectories() (cachePaths, error) {
	values := []struct {
		name string
		path *string
	}{
		{name: "GOCACHE"}, {name: "GOMODCACHE"}, {name: "GOPATH"},
	}
	for index := range values {
		value := os.Getenv(values[index].name)
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n:") {
			return cachePaths{}, nil
		}
		if err := os.MkdirAll(value, 0o700); err != nil {
			return cachePaths{}, err
		}
		if err := os.Chmod(value, 0o700); err != nil {
			return cachePaths{}, err
		}
		info, err := os.Lstat(value)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return cachePaths{}, ErrRejected
		}
		canonical, err := filepath.EvalSymlinks(value)
		if err != nil || canonical != value {
			return cachePaths{}, ErrRejected
		}
		values[index].path = &value
	}
	return cachePaths{goBuild: *values[0].path, goMod: *values[1].path, goPath: *values[2].path}, nil
}

func ensureCacheDirectories(rootFD int, rootDevice uint64, verifiedCWD string) (cachePaths, error) {
	if !filepath.IsAbs(verifiedCWD) {
		return cachePaths{}, ErrRejected
	}
	cacheFD, err := openOrCreateDirectoryAt(rootFD, ".cache", rootDevice)
	if err != nil {
		return cachePaths{}, err
	}
	defer unix.Close(cacheFD)
	cacheRoot := filepath.Join(verifiedCWD, ".cache")
	if err := verifyDirectoryPath(cacheRoot, cacheFD, rootDevice); err != nil {
		return cachePaths{}, err
	}

	result := cachePaths{}
	for _, name := range []string{"go-build", "go-mod", "go-path"} {
		childFD, err := openOrCreateDirectoryAt(cacheFD, name, rootDevice)
		if err != nil {
			return cachePaths{}, err
		}
		absolutePath := filepath.Join(cacheRoot, name)
		if err := verifyDirectoryPath(absolutePath, childFD, rootDevice); err != nil {
			unix.Close(childFD)
			return cachePaths{}, err
		}
		switch name {
		case "go-build":
			result.goBuild = absolutePath
		case "go-mod":
			result.goMod = absolutePath
		case "go-path":
			result.goPath = absolutePath
		}
		if err := unix.Close(childFD); err != nil {
			return cachePaths{}, ErrRejected
		}
	}
	if result.goBuild == "" || result.goMod == "" || result.goPath == "" {
		return cachePaths{}, ErrRejected
	}
	return result, nil
}

func verifyDirectoryPath(path string, openedFD int, rootDevice uint64) error {
	if !filepath.IsAbs(path) {
		return ErrRejected
	}
	pathFD, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrRejected
	}
	defer unix.Close(pathFD)
	var pathStat unix.Stat_t
	var openedStat unix.Stat_t
	if err := unix.Fstat(pathFD, &pathStat); err != nil {
		return ErrRejected
	}
	if err := unix.Fstat(openedFD, &openedStat); err != nil {
		return ErrRejected
	}
	if pathStat.Mode&unix.S_IFMT != unix.S_IFDIR || openedStat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		uint64(pathStat.Dev) != rootDevice || uint64(openedStat.Dev) != rootDevice ||
		uint64(pathStat.Dev) != uint64(openedStat.Dev) || uint64(pathStat.Ino) != uint64(openedStat.Ino) {
		return ErrRejected
	}
	return nil
}

func openOrCreateDirectoryAt(parentFD int, name string, rootDevice uint64) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, ErrRejected
		}
		fd, err = unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, ErrRejected
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(stat.Dev) != rootDevice {
		unix.Close(fd)
		return -1, ErrRejected
	}
	return fd, nil
}

func helperEnvironment() []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"LANG=C",
	}
	if home := os.Getenv("HOME"); home != "" && filepath.IsAbs(home) && filepath.Clean(home) == home {
		environment = append(environment, "HOME="+home)
	}
	return environment
}

func commandEnvironment(preset Preset, cache cachePaths) []string {
	definition, ok := commandDefinitions[preset]
	if !ok || !filepath.IsAbs(cache.goBuild) || !filepath.IsAbs(cache.goMod) || !filepath.IsAbs(cache.goPath) {
		return nil
	}
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GOCACHE=" + cache.goBuild,
		"GOMODCACHE=" + cache.goMod,
		"GOPATH=" + cache.goPath,
	}
	if home := os.Getenv("HOME"); home != "" && filepath.IsAbs(home) && filepath.Clean(home) == home {
		environment = append(environment, "HOME="+home)
	}
	if definition.network {
		environment = append(environment,
			"GOPROXY=https://proxy.golang.org",
			"GOSUMDB=sum.golang.org",
			"GOPRIVATE=",
			"GONOPROXY=none",
			"GONOSUMDB=",
		)
	} else {
		environment = append(environment, "GOPROXY=off")
	}
	return environment
}

type boundedCapture struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (capture *boundedCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	remaining := capture.limit - len(capture.data)
	if remaining > 0 {
		take := len(data)
		if take > remaining {
			take = remaining
		}
		capture.data = append(capture.data, data[:take]...)
	}
	if len(data) > remaining {
		capture.truncated = true
	}
	return len(data), nil
}

func (capture *boundedCapture) Bytes() []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]byte(nil), capture.data...)
}

func (capture *boundedCapture) Truncated() bool {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.truncated
}

var (
	fdPathPattern       = regexp.MustCompile(`/dev/fd/[0-9]+(?:/[^\s"'<>]*)?`)
	absolutePathPattern = regexp.MustCompile(`(^|[\s"'(])/[^\s"'<>)]*`)
	windowsPathPattern  = regexp.MustCompile(`[A-Za-z]:\\[^\s"'<>]*`)
)

func sanitizeDiagnostic(data []byte, redactions []string) (string, bool, bool) {
	if len(data) == 0 {
		return "", false, false
	}
	if !utf8.Valid(data) {
		return "", false, true
	}
	for _, value := range data {
		if value == 0 || (value < 0x20 && value != '\n' && value != '\r' && value != '\t') || value == 0x7f {
			return "", false, true
		}
	}
	diagnostic := string(data)
	lower := strings.ToLower(diagnostic)
	for _, marker := range []string{
		"-----begin private key-----",
		"-----begin rsa private key-----",
		"-----begin openssh private key-----",
		"authorization:",
		"github_pat_",
		"ghp_",
		"sk-proj-",
		"api_key=",
		"password=",
		"token=",
	} {
		if strings.Contains(lower, marker) {
			return "", false, true
		}
	}

	paths := append([]string(nil), redactions...)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, home)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		if path == "" || path == "." || path == string(filepath.Separator) {
			continue
		}
		diagnostic = strings.ReplaceAll(diagnostic, path, "[REDACTED_PATH]")
	}
	diagnostic = fdPathPattern.ReplaceAllString(diagnostic, "[REDACTED_FD]")
	diagnostic = windowsPathPattern.ReplaceAllString(diagnostic, "[REDACTED_PATH]")
	diagnostic = absolutePathPattern.ReplaceAllStringFunc(diagnostic, func(match string) string {
		if len(match) > 0 && strings.ContainsRune(" \t\r\n\"'(", rune(match[0])) {
			return match[:1] + "[REDACTED_PATH]"
		}
		return "[REDACTED_PATH]"
	})
	diagnostic = strings.TrimSpace(diagnostic)
	truncated := false
	if len(diagnostic) > DiagnosticLimit {
		end := DiagnosticLimit
		for end > 0 && !utf8.ValidString(diagnostic[:end]) {
			end--
		}
		diagnostic = diagnostic[:end]
		truncated = true
	}
	return diagnostic, truncated, false
}
