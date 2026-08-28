// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"runtime"
	"testing"
)

func TestAnalysisMemoryLimitUsesTotalAndAvailable(t *testing.T) {
	total := int64(30 * gibibyte)
	if got := AnalysisMemoryLimit(total, total); got != 24*gibibyte {
		t.Fatalf("got %s, want 24.0 GiB", formatBytes(got))
	}
	if got := AnalysisMemoryLimit(total, 20*gibibyte); got != 18*gibibyte {
		t.Fatalf("got %s, want 18.0 GiB", formatBytes(got))
	}
}

func TestAnalysisMemoryLimitKeepsMinimumReserve(t *testing.T) {
	total := int64(16 * gibibyte)
	if got := AnalysisMemoryLimit(total, total); got != 12*gibibyte {
		t.Fatalf("got %s, want 12.0 GiB", formatBytes(got))
	}
}

func TestAnalysisMemoryLimitForObservedMachine(t *testing.T) {
	total := int64(32577777664)
	available := int64(28937281536)
	limit := AnalysisMemoryLimit(total, available)
	if want := total - total/5; limit != want {
		t.Fatalf("got %s, want %s", formatBytes(limit), formatBytes(want))
	}
	if limit > total-4*gibibyte {
		t.Fatalf("analysis limit %s leaves insufficient physical reserve", formatBytes(limit))
	}
}

func TestInitialJobsUsesRequestedConcurrency(t *testing.T) {
	snapshot := MemorySnapshot{Available: 14 * gibibyte, SwapFree: 20 * gibibyte}
	if got := InitialJobs(16, snapshot); got != 16 {
		t.Fatalf("got %d, want 16", got)
	}
	if got := InitialJobs(4, snapshot); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
}

func TestInitialJobsDoesNotThrottleNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 400 * 1024 * 1024, SwapFree: 3 * gibibyte}
	if got := InitialJobs(18, snapshot); got != 18 {
		t.Fatalf("got %d, want 18", got)
	}
}

func TestMaximumJobsRestoresRequestedConcurrency(t *testing.T) {
	if got := MaximumJobs(18); got != 18 {
		t.Fatalf("got %d, want 18", got)
	}
	if got := MaximumJobs(0); got != runtime.NumCPU()+2 {
		t.Fatalf("got %d, want %d", got, runtime.NumCPU()+2)
	}
}

func TestInitialBatchSizeUsesAvailableCapacity(t *testing.T) {
	snapshot := MemorySnapshot{Available: 20 * gibibyte, SwapFree: 24 * gibibyte}
	if got := InitialBatchSize(500, 1600, snapshot); got != 500 {
		t.Fatalf("got %d, want 500", got)
	}
	if got := InitialBatchSize(1200, 1600, snapshot); got != 1200 {
		t.Fatalf("got %d, want 1200", got)
	}
}

func TestInitialBatchSizeDoesNotThrottleNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 400 * 1024 * 1024, SwapFree: 3 * gibibyte}
	if got := InitialBatchSize(500, 1600, snapshot); got != 500 {
		t.Fatalf("got %d, want 500", got)
	}
}

func TestHeavyPoolsBalanceMemoryAndThroughput(t *testing.T) {
	snapshot := MemorySnapshot{Available: 27 * gibibyte, SwapFree: 30 * gibibyte}
	if got := HighmemJobs(18, snapshot); got != 18 {
		t.Fatalf("HighmemJobs() = %d, want 18", got)
	}
	if got := R8Jobs(18, snapshot); got != 18 {
		t.Fatalf("R8Jobs() = %d, want 18", got)
	}
}

func TestHeavyPoolsKeepOneSlotNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 400 * 1024 * 1024, SwapFree: 3 * gibibyte}
	if got := HighmemJobs(18, snapshot); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
}

func TestHighmemJobsHonorsUserLimit(t *testing.T) {
	snapshot := MemorySnapshot{Available: 64 * gibibyte}
	if got := HighmemJobs(6, snapshot); got != 6 {
		t.Fatalf("got %d, want 6", got)
	}
}

func TestCompilerPoolsBalanceMemoryAndThroughput(t *testing.T) {
	snapshot := MemorySnapshot{Available: 27 * gibibyte, SwapFree: 40 * gibibyte}
	if got := JavaJobs(18, snapshot); got != 12 {
		t.Fatalf("JavaJobs() = %d, want 12", got)
	}
	if got := RustJobs(18, snapshot, 4); got != 5 {
		t.Fatalf("RustJobs() = %d, want 5", got)
	}
	if got := KotlinJobs(18, snapshot); got != 6 {
		t.Fatalf("KotlinJobs() = %d, want 6", got)
	}
}

func TestRustPoolAccountsForNestedCodegenParallelism(t *testing.T) {
	snapshot := MemorySnapshot{Available: 27 * gibibyte}
	if got := RustJobs(18, snapshot, 1); got != 12 {
		t.Fatalf("one-CGU RustJobs() = %d, want memory bound 12", got)
	}
	if got := RustJobs(18, snapshot, 16); got != 2 {
		t.Fatalf("sixteen-CGU RustJobs() = %d, want 2", got)
	}
}

func TestCompilerPoolsHonorRequestedJobs(t *testing.T) {
	snapshot := MemorySnapshot{Available: 64 * gibibyte, SwapFree: 40 * gibibyte}
	if got := JavaJobs(6, snapshot); got != 6 {
		t.Fatalf("JavaJobs() = %d, want 6", got)
	}
	if got := KotlinJobs(6, snapshot); got != 6 {
		t.Fatalf("KotlinJobs() = %d, want 6", got)
	}
}

func TestCompilerPoolsKeepOneSlotAtCriticalMemory(t *testing.T) {
	snapshot := MemorySnapshot{Available: 3 * gibibyte, SwapFree: 2 * gibibyte}
	if got := JavaJobs(18, snapshot); got != 1 {
		t.Fatalf("JavaJobs() = %d, want 1", got)
	}
	if got := KotlinJobs(18, snapshot); got != 1 {
		t.Fatalf("KotlinJobs() = %d, want 1", got)
	}
}

func TestObservedGraphPressureOnlyThrottlesHeavyPool(t *testing.T) {
	sample := SegmentSample{
		MinimumAvailable: 857264128,
		MinimumSwapFree:  40461086720,
		MaxPSISomeAvg10:  16.96,
		MaxPSIFullAvg10:  16.57,
		MaxSwapInRate:    19300.1 * 1024 * 1024 / 60,
		MaxSwapOutRate:   34398.9 * 1024 * 1024 / 60,
	}
	batch := InitialBatchSize(4096, 4096, MemorySnapshot{
		Available: sample.MinimumAvailable,
		SwapFree:  sample.MinimumSwapFree,
	})
	if batch != 4096 {
		t.Fatalf("observed graph pressure changed batch=%d", batch)
	}
	if got := HighmemJobs(18, MemorySnapshot{Available: sample.MinimumAvailable, SwapFree: sample.MinimumSwapFree}); got != 1 {
		t.Fatalf("high-memory jobs = %d, want 1", got)
	}
}
