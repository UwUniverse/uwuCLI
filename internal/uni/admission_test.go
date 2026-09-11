// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import "testing"

func TestPoolAdmissionBalancesMemoryAndThroughput(t *testing.T) {
	var admission poolAdmission
	snapshot := MemorySnapshot{Total: 32 * gibibyte, Available: 28 * gibibyte}
	first := admission.decide(18, snapshot, nil, 4)
	if first.highmem != 6 || first.r8 != 5 || first.rust != 5 || first.java != 12 || first.kotlin != 5 {
		t.Fatalf("initial pools = %+v, want 6/5/5/12/5", first)
	}
	if first.reason != "memory-throughput-balance" {
		t.Fatalf("reason = %q", first.reason)
	}
}

func TestPoolAdmissionUsesFreshPhaseMemory(t *testing.T) {
	var admission poolAdmission
	snapshot := MemorySnapshot{Total: 32 * gibibyte, Available: 28 * gibibyte}
	admission.observe(SegmentSample{
		MinimumAvailable: 400 * 1024 * 1024,
		MinimumSwapFree:  gibibyte,
		HighmemJobs:      18,
		R8Jobs:           18,
	})
	second := admission.decide(18, snapshot, nil, 4)
	if second.highmem != 6 || second.r8 != 5 {
		t.Fatalf("recovered pools = %d/%d, want 6/5", second.highmem, second.r8)
	}
}

func TestPoolAdmissionProtectsCurrentCriticalMemory(t *testing.T) {
	var admission poolAdmission
	snapshot := MemorySnapshot{Total: 32 * gibibyte, Available: 400 * 1024 * 1024, SwapFree: gibibyte}
	decision := admission.decide(18, snapshot, nil, 4)
	if decision.highmem != 1 || decision.r8 != 1 || decision.rust != 1 || decision.java != 1 || decision.kotlin != 1 {
		t.Fatalf("critical pools = %+v, want all 1", decision)
	}
	if decision.reason != "memory-throughput-balance" {
		t.Fatalf("reason = %q", decision.reason)
	}
}

func TestPoolAdmissionPreservesExplicitOverrides(t *testing.T) {
	var admission poolAdmission
	snapshot := MemorySnapshot{Total: 32 * gibibyte, Available: 28 * gibibyte}
	decision := admission.decide(18, snapshot, []string{
		"NINJA_HIGHMEM_NUM_JOBS=9",
		"NINJA_UNI_R8_NUM_JOBS=8",
		"NINJA_UNI_RUST_NUM_JOBS=7",
	}, 4)
	if decision.highmem != 9 || decision.r8 != 8 ||
		decision.rust != 7 || !decision.highmemExplicit || !decision.r8Explicit || !decision.rustExplicit {
		t.Fatalf("explicit pools were changed: %+v", decision)
	}
}

func TestPoolAdmissionBacksOffAllAutomaticPoolsOnMemoryRetry(t *testing.T) {
	decision := poolDecision{highmem: 5, r8: 4, rust: 5, java: 11, kotlin: 4}
	first := decision.forMemoryRetry(1)
	if first.highmem != 3 || first.r8 != 2 || first.rust != 3 || first.java != 7 || first.kotlin != 2 {
		t.Fatalf("first retry pools = %+v, want 3/2/3/7/2", first)
	}
	second := decision.forMemoryRetry(2)
	if second.highmem != 2 || second.r8 != 2 || second.rust != 2 || second.java != 5 || second.kotlin != 2 {
		t.Fatalf("second retry pools = %+v, want 2/2/2/5/2", second)
	}
}

func TestPoolAdmissionKeepsExplicitPoolOnMemoryRetry(t *testing.T) {
	decision := poolDecision{highmem: 5, java: 11, highmemExplicit: true}
	retried := decision.forMemoryRetry(1)
	if retried.highmem != 5 || retried.java != 7 {
		t.Fatalf("retry changed explicit pool or failed to scale automatic pool: %+v", retried)
	}
}
