// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import "testing"

func TestPoolAdmissionBalancesMemoryAndThroughput(t *testing.T) {
	var admission poolAdmission
	snapshot := MemorySnapshot{Total: 32 * gibibyte, Available: 28 * gibibyte}
	first := admission.decide(18, snapshot, nil, 4)
	if first.highmem != 18 || first.r8 != 18 || first.rust != 5 || first.java != 12 || first.kotlin != 6 {
		t.Fatalf("initial pools = %+v, want 18/18/5/12/6", first)
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
	if second.highmem != 18 || second.r8 != 18 {
		t.Fatalf("recovered pools = %d/%d, want 18/18", second.highmem, second.r8)
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
