// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"runtime"
	"testing"
)

func TestAnalysisMemoryLimitLeavesTwentyPercentAvailable(t *testing.T) {
	total := int64(30 * gibibyte)
	if got := AnalysisMemoryLimit(total); got != 24*gibibyte {
		t.Fatalf("got %s, want 24.0 GiB", formatBytes(got))
	}
}

func TestAnalysisMemoryLimitKeepsMinimumReserve(t *testing.T) {
	total := int64(16 * gibibyte)
	if got := AnalysisMemoryLimit(total); got != 12*gibibyte {
		t.Fatalf("got %s, want 12.0 GiB", formatBytes(got))
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

func TestInitialJobsBacksOffOnlyNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 900 * 1024 * 1024, SwapFree: 3 * gibibyte}
	if got := InitialJobs(18, snapshot); got != 9 {
		t.Fatalf("got %d, want 9", got)
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
	if got := InitialBatchSize(500, 1600, snapshot); got != 1600 {
		t.Fatalf("got %d, want 1600", got)
	}
	if got := InitialBatchSize(500, 600, snapshot); got != 600 {
		t.Fatalf("got %d, want 600", got)
	}
}

func TestInitialBatchSizeBacksOffNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 900 * 1024 * 1024, SwapFree: 3 * gibibyte}
	if got := InitialBatchSize(500, 1600, snapshot); got != 375 {
		t.Fatalf("got %d, want 375", got)
	}
}

func TestHighmemJobsUsesAvailableMemory(t *testing.T) {
	snapshot := MemorySnapshot{Available: 27 * gibibyte, SwapFree: 30 * gibibyte}
	if got := HighmemJobs(18, snapshot); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
}

func TestHighmemJobsBacksOffNearOOM(t *testing.T) {
	snapshot := MemorySnapshot{Available: 900 * 1024 * 1024, SwapFree: 3 * gibibyte}
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

func TestCompilerPoolsReserveHostMemory(t *testing.T) {
	snapshot := MemorySnapshot{Available: 28 * gibibyte, SwapFree: 40 * gibibyte}
	if got := JavaJobs(18, snapshot); got != 12 {
		t.Fatalf("JavaJobs() = %d, want 12", got)
	}
	if got := KotlinJobs(18, snapshot); got != 6 {
		t.Fatalf("KotlinJobs() = %d, want 6", got)
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

func TestCompilerPoolsKeepOneWorkerUnderPressure(t *testing.T) {
	snapshot := MemorySnapshot{Available: 3 * gibibyte, SwapFree: 2 * gibibyte}
	if got := JavaJobs(18, snapshot); got != 1 {
		t.Fatalf("JavaJobs() = %d, want 1", got)
	}
	if got := KotlinJobs(18, snapshot); got != 1 {
		t.Fatalf("KotlinJobs() = %d, want 1", got)
	}
}

func TestAdaptIgnoresSwapChurnWithoutOOMRisk(t *testing.T) {
	batch, jobs := Adapt(500, 12, 16, SegmentSample{
		MinimumAvailable: 2 * gibibyte,
		MinimumSwapFree:  40 * gibibyte,
		SwapOutBytes:     uint64(gibibyte),
	})
	if batch != 1000 || jobs != 16 {
		t.Fatalf("got batch=%d jobs=%d", batch, jobs)
	}
}

func TestAdaptUnderPressure(t *testing.T) {
	batch, jobs := Adapt(500, 12, 16, SegmentSample{
		MinimumAvailable: 900 * 1024 * 1024,
		MinimumSwapFree:  3 * gibibyte,
	})
	if batch != 375 || jobs != 9 {
		t.Fatalf("got batch=%d jobs=%d", batch, jobs)
	}
}

func TestAdaptWhenHealthy(t *testing.T) {
	batch, jobs := Adapt(500, 8, 10, SegmentSample{
		MinimumAvailable: 8 * gibibyte,
		MinimumSwapFree:  20 * gibibyte,
	})
	if batch != 1000 || jobs != 10 {
		t.Fatalf("got batch=%d jobs=%d", batch, jobs)
	}
}
