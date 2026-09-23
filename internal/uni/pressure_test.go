// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMemoryRetryJobs(t *testing.T) {
	for _, tt := range []struct {
		err, ctxErr         error
		jobs, retries, want int
		retry               bool
	}{
		{errMemoryPressure, nil, 18, 0, 12, true},
		{errMemoryPressure, nil, 12, 1, 8, true},
		{errMemoryPressure, nil, 8, 2, 8, false},
		{errMemoryPressure, nil, 1, 0, 1, false},
		{errMemoryPressure, context.Canceled, 18, 0, 18, false},
		{errors.New("compile failed"), nil, 18, 0, 18, false},
		{nil, nil, 18, 0, 18, false},
	} {
		jobs, retry := memoryRetryJobs(tt.err, tt.ctxErr, tt.jobs, tt.retries)
		if jobs != tt.want || retry != tt.retry {
			t.Fatalf("%+v: got %d, %t", tt, jobs, retry)
		}
	}
}

func TestPressureGuardRejectsInvalidMemory(t *testing.T) {
	var guard memoryPressureGuard
	for _, memory := range []MemorySnapshot{{}, {Total: 32 * gibibyte, Available: -1}} {
		guard.since = time.Unix(1, 0)
		if guard.observe(time.Unix(100, 0), memory, 100, true) || !guard.since.IsZero() {
			t.Fatal("invalid observation caused recovery")
		}
	}
}

func TestPressureGuardRequiresSustainedPressure(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	var guard memoryPressureGuard
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 46, false) {
			t.Fatal("transient pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 46, false) {
		t.Fatal("observed pre-oomd pressure did not stop the build")
	}
}

func TestPressureGuardStopsBeforeOOMDWithAvailableMemory(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 7 * gibibyte}
	var guard memoryPressureGuard
	if guard.observe(start, memory, 40, false) {
		t.Fatal("pressure without concurrent linkers stopped the build")
	}
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 40, true) {
			t.Fatal("transient pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 40, true) {
		t.Fatal("sustained pressure above oomd limit did not stop the build")
	}
}

func TestPressureGuardStopsOOMDPressureWithoutLinkers(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32577777664, Available: 7862628352}
	var guard memoryPressureGuard
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 63.49, false) {
			t.Fatal("transient memory pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 63.49, false) {
		t.Fatal("sustained pressure matching the oomd kill was ignored")
	}
}

func TestPressureGuardResetsAfterRecovery(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	var guard memoryPressureGuard
	guard.observe(start, memory, 25, false)
	if guard.observe(start.Add(10*time.Second), memory, 0, false) {
		t.Fatal("low free memory without stalls is not an OOM prediction")
	}
	if guard.observe(start.Add(12*time.Second), memory, 25, false) {
		t.Fatal("reused stale pressure interval")
	}
}

func TestPressureGuardAllowsHealthyParallelism(t *testing.T) {
	var guard memoryPressureGuard
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 12 * gibibyte}
	for i := 0; i < 100; i++ {
		if guard.observe(time.Unix(int64(i+100), 0), memory, 40, true) {
			t.Fatal("plenty of available memory should not stop the build")
		}
	}
}

func TestPressureGuardAllowsExtremePSIWithFreeMemory(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32577765376, Available: 17234452480}
	var guard memoryPressureGuard
	for i := 0; i < 30; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 65.60, true) {
			t.Fatal("PSI alone stopped the build despite available memory")
		}
	}
	memory.Available = 2 * gibibyte
	if guard.observe(start.Add(time.Minute), memory, 65.60, true) {
		t.Fatal("high-PSI samples with free memory started the low-memory timer")
	}
	if !guard.observe(start.Add(time.Minute+6*time.Second), memory, 65.60, true) {
		t.Fatal("sustained low memory no longer stops the build")
	}
}

func TestPressureGuardStopsMemoryExhaustionWithoutPSI(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: gibibyte / 4}
	var guard memoryPressureGuard
	if guard.observe(start, memory, 0, false) {
		t.Fatal("single observation stopped the build")
	}
	if !guard.observe(start.Add(6*time.Second), memory, 0, false) {
		t.Fatal("memory exhaustion without PSI did not stop the build")
	}
}

func TestMemoryRecoveryGuardRequiresStableRecovery(t *testing.T) {
	healthy := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	var guard memoryRecoveryGuard
	for i := 1; i < memoryRecoverySamples; i++ {
		if guard.observe(healthy, memoryRecoveryPSI) {
			t.Fatal("unstable recovery was accepted")
		}
	}
	if !guard.observe(healthy, memoryRecoveryPSI) {
		t.Fatal("stable recovery was not accepted")
	}
}

func TestMemoryRecoveryGuardResetsOnPressure(t *testing.T) {
	healthy := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	pressured := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	var guard memoryRecoveryGuard
	guard.observe(healthy, 0)
	guard.observe(healthy, 0)
	if guard.observe(pressured, 0) {
		t.Fatal("low memory was accepted as recovered")
	}
	if guard.observe(healthy, 0) {
		t.Fatal("recovery samples survived renewed pressure")
	}
	if guard.observe(healthy, memoryRecoveryPSI+1) {
		t.Fatal("high PSI was accepted as recovered")
	}
}

func TestReplaceParallelArgsPreservesBuildInputs(t *testing.T) {
	for _, args := range [][]string{
		{"-j18", "Settings", "A=B"},
		{"-j", "18", "Settings", "A=B"},
		{"Settings", "A=B"},
	} {
		if got := replaceParallelArgs(args, 12); !reflect.DeepEqual(got, []string{"-j12", "Settings", "A=B"}) {
			t.Fatalf("args %v became %v", args, got)
		}
	}
	if got := replaceParallelArgs([]string{"--", "-j18"}, 12); !reflect.DeepEqual(got, []string{"-j12", "--", "-j18"}) {
		t.Fatalf("modified target after --: %v", got)
	}
}
