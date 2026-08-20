// Package scheduler provides deterministic, resource-aware local execution.
package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"
)

var (
	ErrRejected      = errors.New("scheduler request rejected")
	ErrBackpressure  = errors.New("scheduler queue full")
	ErrCycle         = errors.New("scheduler dependency cycle")
	ErrUnschedulable = errors.New("scheduler job cannot be admitted")
	ErrHostPressure  = errors.New("scheduler host pressure denies admission")
)

type Resources struct {
	CPU         int
	MemoryBytes int64
}

type Quota struct {
	CPU         int
	MemoryBytes int64
	Concurrent  int
}

type HostPressure struct {
	AvailableCPU         int
	AvailableMemoryBytes int64
}

type JobClass struct {
	Weight        int
	MaxConcurrent int
}

const (
	ClassBuild       = "build"
	ClassTest        = "test"
	ClassVerify      = "verify"
	ClassInteractive = "interactive"
)

type Job struct {
	ID        string
	TaskID    string
	RuntimeID string
	Class     string
	Resources Resources
	DependsOn []string
	Run       func(context.Context) error
}

type Config struct {
	Capacity        Resources
	MaxConcurrent   int
	MaxQueued       int
	Classes         map[string]JobClass
	PerTaskQuota    Quota
	PerRuntimeQuota Quota
	Pressure        func(context.Context) (HostPressure, error)
}

type Scheduler struct {
	config Config
	mu     sync.Mutex
	queued []Job
	run    bool
}

type result struct {
	id  string
	err error
}

type usage struct {
	resources  Resources
	concurrent int
}

func New(config Config) (*Scheduler, error) {
	if config.Capacity.CPU <= 0 || config.Capacity.MemoryBytes <= 0 || config.MaxConcurrent <= 0 || config.MaxQueued <= 0 {
		return nil, ErrRejected
	}
	if config.PerTaskQuota.Concurrent == 0 {
		config.PerTaskQuota.Concurrent = config.MaxConcurrent
	}
	if config.PerRuntimeQuota.Concurrent == 0 {
		config.PerRuntimeQuota.Concurrent = config.MaxConcurrent
	}
	if config.PerTaskQuota.CPU == 0 {
		config.PerTaskQuota.CPU = config.Capacity.CPU
	}
	if config.PerTaskQuota.MemoryBytes == 0 {
		config.PerTaskQuota.MemoryBytes = config.Capacity.MemoryBytes
	}
	if config.PerRuntimeQuota.CPU == 0 {
		config.PerRuntimeQuota.CPU = config.Capacity.CPU
	}
	if config.PerRuntimeQuota.MemoryBytes == 0 {
		config.PerRuntimeQuota.MemoryBytes = config.Capacity.MemoryBytes
	}
	for name, class := range config.Classes {
		if name == "" || class.Weight <= 0 || class.MaxConcurrent < 0 {
			return nil, ErrRejected
		}
	}
	return &Scheduler{config: config}, nil
}

func (s *Scheduler) Submit(ctx context.Context, job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || ctx.Err() != nil || s.run || !validJob(job) {
		return ErrRejected
	}
	if len(s.queued) >= s.config.MaxQueued {
		return ErrBackpressure
	}
	for _, queued := range s.queued {
		if queued.ID == job.ID {
			return ErrRejected
		}
	}
	s.queued = append(s.queued, job)
	return nil
}

func (s *Scheduler) RunSubmitted(ctx context.Context) error {
	s.mu.Lock()
	if s.run {
		s.mu.Unlock()
		return ErrRejected
	}
	s.run = true
	jobs := append([]Job(nil), s.queued...)
	s.queued = nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.run = false
		s.mu.Unlock()
	}()
	return s.Run(ctx, jobs)
}

// Run executes a closed job graph. It launches only jobs whose dependencies
// are complete and whose CPU/RAM, class, task, runtime, and host-pressure
// budgets all admit them.
func (s *Scheduler) Run(ctx context.Context, jobs []Job) error {
	if ctx == nil || len(jobs) == 0 {
		if ctx == nil {
			return ErrRejected
		}
		return nil
	}
	if len(jobs) > s.config.MaxQueued {
		return ErrBackpressure
	}
	if err := validateGraph(jobs); err != nil {
		return err
	}
	byID := make(map[string]Job, len(jobs))
	state := make(map[string]string, len(jobs))
	for _, job := range jobs {
		byID[job.ID] = job
		state[job.ID] = "pending"
	}
	baseCtx, cancel := context.WithCancel(ctx)
	group, runCtx := errgroup.WithContext(baseCtx)
	defer cancel()
	results := make(chan result, len(jobs))
	active := 0
	completed := 0
	classServed := map[string]int{}
	classRunning := map[string]int{}
	taskUsage := map[string]usage{}
	runtimeUsage := map[string]usage{}
	used := Resources{}
	var firstErr error

	for completed < len(jobs) {
		if runCtx.Err() != nil {
			if firstErr == nil {
				firstErr = runCtx.Err()
			}
			cancel()
		}
		for active < s.config.MaxConcurrent {
			ready := readyJobs(byID, state)
			job, ok := s.chooseReady(ready, classServed, classRunning, taskUsage, runtimeUsage, used, runCtx)
			if !ok {
				break
			}
			state[job.ID] = "running"
			active++
			classRunning[job.Class]++
			classServed[job.Class]++
			used = addResources(used, job.Resources)
			taskUsage[job.TaskID] = addUsage(taskUsage[job.TaskID], job.Resources)
			runtimeUsage[job.RuntimeID] = addUsage(runtimeUsage[job.RuntimeID], job.Resources)
			currentJob := job
			group.Go(func() error {
				err := currentJob.Run(runCtx)
				results <- result{id: currentJob.ID, err: err}
				return err
			})
		}
		if active == 0 {
			if completed == len(jobs) {
				break
			}
			if hasReadyJobs(readyJobs(byID, state)) {
				if firstErr == nil {
					firstErr = s.admissionError(runCtx, readyJobs(byID, state))
				}
			}
			cancel()
			if firstErr == nil {
				firstErr = ErrUnschedulable
			}
			break
		}
		select {
		case item := <-results:
			active--
			job := byID[item.id]
			state[item.id] = "done"
			used = subtractResources(used, job.Resources)
			taskUsage[job.TaskID] = subtractUsage(taskUsage[job.TaskID], job.Resources)
			runtimeUsage[job.RuntimeID] = subtractUsage(runtimeUsage[job.RuntimeID], job.Resources)
			classRunning[job.Class]--
			completed++
			if item.err != nil && firstErr == nil {
				firstErr = item.err
				cancel()
			}
		case <-runCtx.Done():
			if firstErr == nil {
				firstErr = runCtx.Err()
			}
		}
	}
	for active > 0 {
		item := <-results
		active--
		if item.err != nil && firstErr == nil {
			firstErr = item.err
		}
	}
	if groupErr := group.Wait(); firstErr == nil {
		firstErr = groupErr
	}
	return firstErr
}

func (s *Scheduler) chooseReady(ready []Job, served, running map[string]int, taskUsage, runtimeUsage map[string]usage, used Resources, ctx context.Context) (Job, bool) {
	if len(ready) == 0 {
		return Job{}, false
	}
	sort.SliceStable(ready, func(i, j int) bool {
		left := s.class(ready[i].Class)
		right := s.class(ready[j].Class)
		leftScore := float64(served[ready[i].Class]) / float64(left.Weight)
		rightScore := float64(served[ready[j].Class]) / float64(right.Weight)
		if leftScore != rightScore {
			return leftScore < rightScore
		}
		return ready[i].ID < ready[j].ID
	})
	for _, job := range ready {
		class := s.class(job.Class)
		if class.MaxConcurrent > 0 && running[job.Class] >= class.MaxConcurrent {
			continue
		}
		if s.config.MaxConcurrent <= 0 || used.CPU+job.Resources.CPU > s.config.Capacity.CPU || used.MemoryBytes+job.Resources.MemoryBytes > s.config.Capacity.MemoryBytes {
			continue
		}
		if !withinQuota(taskUsage[job.TaskID], job.Resources, s.config.PerTaskQuota) || !withinQuota(runtimeUsage[job.RuntimeID], job.Resources, s.config.PerRuntimeQuota) {
			continue
		}
		if s.config.Pressure != nil {
			pressure, err := s.config.Pressure(ctx)
			if err != nil || pressure.AvailableCPU < job.Resources.CPU || pressure.AvailableMemoryBytes < job.Resources.MemoryBytes {
				continue
			}
		}
		return job, true
	}
	return Job{}, false
}

func (s *Scheduler) admissionError(ctx context.Context, ready []Job) error {
	for _, job := range ready {
		if job.Resources.CPU > s.config.Capacity.CPU || job.Resources.MemoryBytes > s.config.Capacity.MemoryBytes {
			return ErrUnschedulable
		}
		if s.config.Pressure != nil {
			pressure, err := s.config.Pressure(ctx)
			if err == nil && (pressure.AvailableCPU < job.Resources.CPU || pressure.AvailableMemoryBytes < job.Resources.MemoryBytes) {
				return ErrHostPressure
			}
		}
	}
	return ErrUnschedulable
}

func (s *Scheduler) class(name string) JobClass {
	if class, ok := s.config.Classes[name]; ok {
		return class
	}
	return JobClass{Weight: 1}
}

func validateGraph(jobs []Job) error {
	byID := map[string]Job{}
	for _, job := range jobs {
		if !validJob(job) {
			return ErrRejected
		}
		if _, exists := byID[job.ID]; exists {
			return ErrRejected
		}
		byID[job.ID] = job
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return ErrCycle
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dep := range byID[id].DependsOn {
			if _, ok := byID[dep]; !ok {
				return ErrRejected
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func readyJobs(byID map[string]Job, state map[string]string) []Job {
	ready := []Job{}
	for id, job := range byID {
		if state[id] != "pending" {
			continue
		}
		readyNow := true
		for _, dep := range job.DependsOn {
			if state[dep] != "done" {
				readyNow = false
				break
			}
		}
		if readyNow {
			ready = append(ready, job)
		}
	}
	return ready
}

func hasReadyJobs(jobs []Job) bool { return len(jobs) > 0 }

func validJob(job Job) bool {
	return job.ID != "" && job.TaskID != "" && job.RuntimeID != "" && job.Class != "" && job.Resources.CPU > 0 && job.Resources.MemoryBytes > 0 && job.Run != nil
}

func withinQuota(current usage, requested Resources, quota Quota) bool {
	return current.resources.CPU+requested.CPU <= quota.CPU && current.resources.MemoryBytes+requested.MemoryBytes <= quota.MemoryBytes && current.concurrent+1 <= quota.Concurrent
}

func addResources(left, right Resources) Resources {
	return Resources{CPU: left.CPU + right.CPU, MemoryBytes: left.MemoryBytes + right.MemoryBytes}
}
func subtractResources(left, right Resources) Resources {
	return Resources{CPU: left.CPU - right.CPU, MemoryBytes: left.MemoryBytes - right.MemoryBytes}
}
func addUsage(current usage, resources Resources) usage {
	current.resources = addResources(current.resources, resources)
	current.concurrent++
	return current
}
func subtractUsage(current usage, resources Resources) usage {
	current.resources = subtractResources(current.resources, resources)
	current.concurrent--
	return current
}
