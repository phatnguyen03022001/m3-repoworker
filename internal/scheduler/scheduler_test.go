package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testScheduler(t *testing.T, pressure func(context.Context) (HostPressure, error)) *Scheduler {
	t.Helper()
	scheduler, err := New(Config{
		Capacity: Resources{CPU: 2, MemoryBytes: 256 << 20}, MaxConcurrent: 2, MaxQueued: 16,
		Classes:         map[string]JobClass{ClassBuild: {Weight: 1}, ClassTest: {Weight: 2}},
		PerTaskQuota:    Quota{CPU: 2, MemoryBytes: 256 << 20, Concurrent: 2},
		PerRuntimeQuota: Quota{CPU: 2, MemoryBytes: 256 << 20, Concurrent: 2}, Pressure: pressure,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return scheduler
}

func makeJob(id, task, runtime, class string, cpu int, deps []string, run func(context.Context) error) Job {
	return Job{ID: id, TaskID: task, RuntimeID: runtime, Class: class, Resources: Resources{CPU: cpu, MemoryBytes: 64 << 20}, DependsOn: deps, Run: run}
}

func TestSchedulerHonorsDAGResourcesAndQuotas(t *testing.T) {
	scheduler := testScheduler(t, nil)
	var mu sync.Mutex
	active := 0
	maxActive := 0
	started := map[string]bool{}
	run := func(id string) func(context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			started[id] = true
			mu.Unlock()
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			mu.Lock()
			active--
			mu.Unlock()
			return nil
		}
	}
	jobs := []Job{
		makeJob("a", "task-a", "runtime-a", ClassBuild, 1, nil, run("a")),
		makeJob("b", "task-a", "runtime-a", ClassTest, 1, nil, run("b")),
		makeJob("c", "task-a", "runtime-a", ClassTest, 1, []string{"a", "b"}, run("c")),
	}
	if err := scheduler.Run(context.Background(), jobs); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if maxActive > 2 || len(started) != 3 {
		t.Fatalf("active=%d started=%v", maxActive, started)
	}
}

func TestSchedulerRejectsCycleAndBackpressure(t *testing.T) {
	scheduler := testScheduler(t, nil)
	cycle := []Job{
		makeJob("a", "task", "runtime", ClassBuild, 1, []string{"b"}, func(context.Context) error { return nil }),
		makeJob("b", "task", "runtime", ClassBuild, 1, []string{"a"}, func(context.Context) error { return nil }),
	}
	if err := scheduler.Run(context.Background(), cycle); !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle error = %v", err)
	}
	for i := 0; i < 16; i++ {
		if err := scheduler.Submit(context.Background(), makeJob(fmt.Sprintf("job-%d", i), "task", "runtime", ClassBuild, 1, nil, func(context.Context) error { return nil })); err != nil {
			t.Fatalf("Submit(%d) error = %v", i, err)
		}
	}
	if err := scheduler.Submit(context.Background(), makeJob("overflow", "task", "runtime", ClassBuild, 1, nil, func(context.Context) error { return nil })); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("backpressure error = %v", err)
	}
}

func TestSchedulerFailureCancelsSiblingJobs(t *testing.T) {
	scheduler := testScheduler(t, nil)
	started := make(chan struct{})
	canceled := make(chan struct{})
	jobs := []Job{
		makeJob("fail", "task", "runtime", ClassBuild, 1, nil, func(context.Context) error { return errors.New("boom") }),
		makeJob("wait", "task", "runtime", ClassTest, 1, nil, func(ctx context.Context) error { close(started); <-ctx.Done(); close(canceled); return ctx.Err() }),
	}
	err := scheduler.Run(context.Background(), jobs)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("failure Run() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sibling never started")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("sibling was not canceled")
	}
}

func TestSchedulerHostPressureAndUnschedulableJob(t *testing.T) {
	scheduler := testScheduler(t, func(context.Context) (HostPressure, error) {
		return HostPressure{AvailableCPU: 0, AvailableMemoryBytes: 0}, nil
	})
	job := makeJob("pressure", "task", "runtime", ClassBuild, 1, nil, func(context.Context) error { return nil })
	if err := scheduler.Run(context.Background(), []Job{job}); !errors.Is(err, ErrHostPressure) {
		t.Fatalf("pressure error = %v", err)
	}
	tooLarge := job
	tooLarge.ID = "too-large"
	tooLarge.Resources.CPU = 3
	scheduler = testScheduler(t, nil)
	if err := scheduler.Run(context.Background(), []Job{tooLarge}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("unschedulable error = %v", err)
	}
}

func TestSchedulerRunSubmittedAndFairClassSelection(t *testing.T) {
	scheduler := testScheduler(t, nil)
	var mu sync.Mutex
	order := []string{}
	for index, class := range []string{ClassTest, ClassBuild, ClassTest} {
		id := fmt.Sprintf("%s-%d", class, index)
		job := makeJob(id, id, id, class, 1, nil, func(context.Context) error { mu.Lock(); order = append(order, id); mu.Unlock(); return nil })
		if err := scheduler.Submit(context.Background(), job); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}
	if err := scheduler.RunSubmitted(context.Background()); err != nil {
		t.Fatalf("RunSubmitted() error = %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("order = %#v", order)
	}
}
