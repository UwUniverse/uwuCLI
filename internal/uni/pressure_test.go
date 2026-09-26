// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestMemoryRetryJobs(t *testing.T) {
	for _, tt := range []struct {
		err, ctxErr error
		jobs, want  int
		retry       bool
	}{
		{errMemoryPressure, nil, 18, 12, true},
		{errMemoryPressure, nil, 12, 8, true},
		{errMemoryPressure, nil, 8, 5, true},
		{errMemoryPressure, nil, 1, 1, true},
		{errMemoryPressure, context.Canceled, 18, 18, false},
		{errors.New("compile failed"), nil, 18, 18, false},
		{nil, nil, 18, 18, false},
	} {
		jobs, retry := memoryRetryJobs(tt.err, tt.ctxErr, tt.jobs)
		if jobs != tt.want || retry != tt.retry {
			t.Fatalf("%+v: got %d, %t", tt, jobs, retry)
		}
	}
}

func TestRunaParallelismRampRaisesGraduallyAndStopsAtTwoPointFiveTimesInferredJobs(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	var ramp runaParallelismRamp
	if jobs, ceiling, raised := ramp.observe(start, memory, 0, swapRates{}, 18, 18); raised || jobs != 18 || ceiling != 45 {
		t.Fatalf("initial ramp observation = %d/%d/%t, want 18/45/false", jobs, ceiling, raised)
	}
	if jobs, _, raised := ramp.observe(start.Add(runaParallelismRampInterval-time.Second), memory, 0, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatalf("early ramp observation = %d/%t, want 18/false", jobs, raised)
	}
	jobs, ceiling, raised := ramp.observe(start.Add(runaParallelismRampInterval), memory, 0, swapRates{}, 18, 18)
	if !raised || jobs != 21 || ceiling != 45 {
		t.Fatalf("first ramp = %d/%d/%t, want 21/45/true", jobs, ceiling, raised)
	}
	now := start.Add(10 * runaParallelismRampInterval)
	for jobs < ceiling {
		var increase bool
		jobs, ceiling, increase = ramp.observe(now, memory, 0, swapRates{}, jobs, 18)
		if !increase && jobs < ceiling {
			t.Fatalf("ramp stalled below ceiling at %d", jobs)
		}
		now = now.Add(runaParallelismRampInterval)
	}
	if jobs != 45 {
		t.Fatalf("ramp exceeded or missed ceiling: got %d, want 45", jobs)
	}
	if next, gotCeiling, increase := ramp.observe(now.Add(runaParallelismRampInterval), memory, 0, swapRates{}, jobs, 18); increase || next != 45 || gotCeiling != 45 {
		t.Fatalf("at ceiling ramp = %d/%d/%t, want 45/45/false", next, gotCeiling, increase)
	}
	if ceiling := runaParallelismCeiling(17); ceiling != 42 {
		t.Fatalf("odd inferred job ceiling = %d, want 42", ceiling)
	}
}

func TestRunaParallelismRampRequiresHealthyMemoryAndPSI(t *testing.T) {
	start := time.Unix(100, 0)
	var ramp runaParallelismRamp
	healthy := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	if jobs, _, raised := ramp.observe(start, healthy, 0, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp increased without a sustained healthy interval")
	}
	pressured := MemorySnapshot{Total: 32 * gibibyte, Available: 5 * gibibyte}
	if jobs, _, raised := ramp.observe(start.Add(runaParallelismRampInterval), pressured, 0, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp increased with limited available memory")
	}
	if jobs, _, raised := ramp.observe(start.Add(2*runaParallelismRampInterval), healthy, runaParallelismRampMaxPSI, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp increased with high memory PSI")
	}
	if jobs, _, raised := ramp.observe(start.Add(3*runaParallelismRampInterval), healthy, 0, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp reused an old healthy interval after pressure")
	}
}

func TestRunaParallelismRampWaitsForSwapActivityToClear(t *testing.T) {
	start := time.Unix(100, 0)
	healthy := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	activeSwap := swapRates{inBytesPerSecond: runaSwapInPressureRate}
	var ramp runaParallelismRamp
	ramp.observe(start, healthy, 0, swapRates{}, 18, 18)
	if jobs, _, raised := ramp.observe(start.Add(runaParallelismRampInterval), healthy, 0, activeSwap, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp increased during active swap-in")
	}
	if jobs, _, raised := ramp.observe(start.Add(2*runaParallelismRampInterval), healthy, 0, swapRates{}, 18, 18); raised || jobs != 18 {
		t.Fatal("ramp reused healthy time from before swap activity")
	}
	if jobs, _, raised := ramp.observe(start.Add(3*runaParallelismRampInterval), healthy, 0, swapRates{}, 18, 18); !raised || jobs != 21 {
		t.Fatalf("ramp did not resume after sustained swap-free interval: %d/%t", jobs, raised)
	}
}

func TestSwapRateSamplerCalculatesRatesAndResetsCounters(t *testing.T) {
	start := time.Unix(100, 0)
	pageSize := uint64(os.Getpagesize())
	var sampler swapRateSampler
	first := MemorySnapshot{SwapInPage: 100, SwapOutPage: 200}
	if rates := sampler.observe(start, first); rates.active() {
		t.Fatal("baseline sample reported swap activity")
	}
	second := MemorySnapshot{SwapInPage: 100 + 2*1024*1024/pageSize, SwapOutPage: 200 + 8*1024*1024/pageSize}
	rates := sampler.observe(start.Add(time.Second), second)
	if math.Abs(rates.inBytesPerSecond-2*1024*1024) > float64(pageSize) || math.Abs(rates.outBytesPerSecond-8*1024*1024) > float64(pageSize) {
		t.Fatalf("calculated swap rates = %.0f/%.0f bytes/s", rates.inBytesPerSecond, rates.outBytesPerSecond)
	}
	if rates.active() {
		t.Fatal("swap-out below the configured pressure threshold was considered active")
	}
	reset := MemorySnapshot{SwapInPage: 1, SwapOutPage: 1}
	if rates := sampler.observe(start.Add(2*time.Second), reset); rates.active() {
		t.Fatal("counter reset produced a bogus high swap rate")
	}
	if rates := sampler.observe(start.Add(3*time.Second), MemorySnapshot{SwapInPage: 1, SwapOutPage: 1 + 16*1024*1024/pageSize}); !rates.active() {
		t.Fatal("high swap-out rate was not detected")
	}
}

func TestPressureGuardRejectsInvalidMemory(t *testing.T) {
	var guard memoryPressureGuard
	for _, memory := range []MemorySnapshot{{}, {Total: 32 * gibibyte, Available: -1}} {
		guard.since = time.Unix(1, 0)
		if guard.observe(time.Unix(100, 0), memory, 100, swapRates{}, true) || !guard.since.IsZero() {
			t.Fatal("invalid observation caused recovery")
		}
	}
}

func TestPressureGuardRequiresSustainedPressure(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	var guard memoryPressureGuard
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 46, swapRates{}, false) {
			t.Fatal("transient pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 46, swapRates{}, false) {
		t.Fatal("observed pre-oomd pressure did not stop the build")
	}
}

func TestPressureGuardStopsBeforeOOMDWithAvailableMemory(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 7 * gibibyte}
	var guard memoryPressureGuard
	if guard.observe(start, memory, 40, swapRates{}, false) {
		t.Fatal("pressure without concurrent linkers stopped the build")
	}
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 40, swapRates{}, true) {
			t.Fatal("transient pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 40, swapRates{}, true) {
		t.Fatal("sustained pressure above oomd limit did not stop the build")
	}
}

func TestPressureGuardUsesSwapOnlyWithLowMemoryAndPSI(t *testing.T) {
	start := time.Unix(100, 0)
	activeSwap := swapRates{inBytesPerSecond: runaSwapInPressureRate}
	var guard memoryPressureGuard
	plenty := MemorySnapshot{Total: 32 * gibibyte, Available: 20 * gibibyte}
	if guard.observe(start, plenty, 15, activeSwap, false) {
		t.Fatal("swap activity stopped the build despite free memory")
	}
	low := MemorySnapshot{Total: 32 * gibibyte, Available: 8 * gibibyte}
	if guard.observe(start.Add(2*time.Second), low, 5, activeSwap, false) {
		t.Fatal("swap activity alone started pressure response without PSI stalls")
	}
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(4+2*i)*time.Second), low, 15, activeSwap, false) {
			t.Fatal("transient swap pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(10*time.Second), low, 15, activeSwap, false) {
		t.Fatal("sustained low-memory PSI and swap activity was ignored")
	}
}

func TestPressureGuardStopsOOMDPressureWithoutLinkers(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32577777664, Available: 7862628352}
	var guard memoryPressureGuard
	for i := 0; i < 3; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 63.49, swapRates{}, false) {
			t.Fatal("transient memory pressure stopped the build")
		}
	}
	if !guard.observe(start.Add(6*time.Second), memory, 63.49, swapRates{}, false) {
		t.Fatal("sustained pressure matching the oomd kill was ignored")
	}
}

func TestPressureGuardResetsAfterRecovery(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 2 * gibibyte}
	var guard memoryPressureGuard
	guard.observe(start, memory, 25, swapRates{}, false)
	if guard.observe(start.Add(10*time.Second), memory, 0, swapRates{}, false) {
		t.Fatal("low free memory without stalls is not an OOM prediction")
	}
	if guard.observe(start.Add(12*time.Second), memory, 25, swapRates{}, false) {
		t.Fatal("reused stale pressure interval")
	}
}

func TestPressureGuardAllowsHealthyParallelism(t *testing.T) {
	var guard memoryPressureGuard
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: 12 * gibibyte}
	for i := 0; i < 100; i++ {
		if guard.observe(time.Unix(int64(i+100), 0), memory, 40, swapRates{}, true) {
			t.Fatal("plenty of available memory should not stop the build")
		}
	}
}

func TestPressureGuardAllowsExtremePSIWithFreeMemory(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32577765376, Available: 17234452480}
	var guard memoryPressureGuard
	for i := 0; i < 30; i++ {
		if guard.observe(start.Add(time.Duration(i)*2*time.Second), memory, 65.60, swapRates{}, true) {
			t.Fatal("PSI alone stopped the build despite available memory")
		}
	}
	memory.Available = 2 * gibibyte
	if guard.observe(start.Add(time.Minute), memory, 65.60, swapRates{}, true) {
		t.Fatal("high-PSI samples with free memory started the low-memory timer")
	}
	if !guard.observe(start.Add(time.Minute+6*time.Second), memory, 65.60, swapRates{}, true) {
		t.Fatal("sustained low memory no longer stops the build")
	}
}

func TestPressureGuardStopsMemoryExhaustionWithoutPSI(t *testing.T) {
	start := time.Unix(100, 0)
	memory := MemorySnapshot{Total: 32 * gibibyte, Available: gibibyte / 4}
	var guard memoryPressureGuard
	if guard.observe(start, memory, 0, swapRates{}, false) {
		t.Fatal("single observation stopped the build")
	}
	if !guard.observe(start.Add(6*time.Second), memory, 0, swapRates{}, false) {
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
