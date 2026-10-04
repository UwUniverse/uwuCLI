// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testParallelismControl struct {
	requests []int
	err      error
}

func (control *testParallelismControl) setParallelism(_ context.Context, jobs int) (string, error) {
	control.requests = append(control.requests, jobs)
	return "accepted", control.err
}

func TestAdaptiveParallelismIgnoresObservedModeratePressure(t *testing.T) {
	state := adaptiveParallelism{jobs: 18, ceiling: 18}
	control := &testParallelismControl{}
	memory := MemorySnapshot{Total: 32577765376, Available: 3279462400}
	for i := 0; i < 60; i++ {
		change := state.observe(context.Background(), time.Unix(100+int64(i*2), 0),
			memory, 10.77, swapRates{outBytesPerSecond: 128 * 1024 * 1024}, false, control)
		if change.jobs != 18 || change.err != nil {
			t.Fatalf("observed moderate stalls changed concurrency: %+v", change)
		}
	}
	if len(control.requests) != 0 {
		t.Fatal("moderate stalls sent a control request")
	}
}

func TestAdaptiveParallelismLowersAdmissionAndRetriesMissingControl(t *testing.T) {
	state := adaptiveParallelism{jobs: 18, ceiling: 18}
	control := &testParallelismControl{err: errors.New("socket unavailable")}
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	start := time.Unix(100, 0)
	state.observe(context.Background(), start, memory, 46, swapRates{}, false, control)
	change := state.observe(context.Background(), start.Add(6*time.Second), memory, 46, swapRates{}, false, control)
	if change.err == nil || state.jobs != 18 {
		t.Fatal("missing control was treated as acknowledged")
	}
	control.err = nil
	state.observe(context.Background(), start.Add(8*time.Second), memory, 46, swapRates{}, false, control)
	if len(control.requests) != 1 {
		t.Fatal("control retry was not rate-limited")
	}
	change = state.observe(context.Background(), start.Add(16*time.Second), memory, 46, swapRates{}, false, control)
	if change.err != nil || change.jobs != 14 || state.jobs != 14 {
		t.Fatalf("reconnected control did not lower admission: %+v", change)
	}
}

func TestAdaptiveParallelismGraduallyRecoversWithinOneAndHalfTimesUserCeiling(t *testing.T) {
	state := adaptiveParallelism{jobs: 14, ceiling: 18}
	control := &testParallelismControl{}
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 16 * gibibyte}
	start := time.Unix(100, 0)
	for i := 0; i < 14; i++ {
		state.observe(context.Background(), start.Add(time.Duration(i)*time.Second), memory, 0, swapRates{}, false, control)
	}
	if state.jobs != 14 {
		t.Fatal("concurrency was restored before stable recovery")
	}
	state.observe(context.Background(), start.Add(15*time.Second), memory, 0, swapRates{}, false, control)
	if state.jobs != 17 {
		t.Fatal("recovery did not use a bounded step")
	}
	for _, elapsed := range []time.Duration{30, 45, 60, 75, 90} {
		state.observe(context.Background(), start.Add(elapsed*time.Second), memory, 0, swapRates{}, false, control)
	}
	if state.jobs != 27 || len(control.requests) != 5 {
		t.Fatalf("recovery did not reach 1.5x the user's requested jobs: jobs=%d requests=%v", state.jobs, control.requests)
	}
}

func TestAdaptiveParallelismWithoutControlKeepsRunningTasks(t *testing.T) {
	state := adaptiveParallelism{jobs: 18, ceiling: 18}
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: gibibyte / 4}
	start := time.Unix(100, 0)
	state.observe(context.Background(), start, memory, 0, swapRates{}, false, nil)
	change := state.observe(context.Background(), start.Add(2*time.Second), memory, 0, swapRates{}, false, nil)
	if change.err == nil || state.jobs != 18 {
		t.Fatal("unsupported executor was silently changed")
	}
}

func TestAdaptiveParallelismCooldownAndMinimum(t *testing.T) {
	state := adaptiveParallelism{jobs: 2, ceiling: 18}
	control := &testParallelismControl{}
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: gibibyte / 4}
	start := time.Unix(100, 0)
	for i := 0; i < 30; i++ {
		state.observe(context.Background(), start.Add(time.Duration(i)*time.Second), memory, 0, swapRates{}, false, control)
	}
	if state.jobs != 1 || len(control.requests) != 1 {
		t.Fatalf("minimum concurrency or cooldown failed: jobs=%d requests=%v", state.jobs, control.requests)
	}
}
