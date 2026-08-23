// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"reflect"
	"testing"
	"time"
)

func TestOutputLockRejectsConcurrentScheduler(t *testing.T) {
	outDir := t.TempDir()
	first, err := acquireOutputLock(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	if second, err := acquireOutputLock(outDir); err == nil {
		second.close()
		t.Fatal("concurrent output lock was accepted")
	}
}

func TestFormatBuildDuration(t *testing.T) {
	tests := map[time.Duration]string{
		7 * time.Second:                                                     "7 seconds",
		20*time.Minute + 28*time.Second:                                     "20:28 (mm:ss)",
		2*time.Hour + 11*time.Minute + 4*time.Second:                        "02:11:04 (hh:mm:ss)",
		2*time.Hour + 11*time.Minute + 4*time.Second + 900*time.Millisecond: "02:11:04 (hh:mm:ss)",
	}
	for elapsed, want := range tests {
		if got := formatBuildDuration(elapsed); got != want {
			t.Fatalf("formatBuildDuration(%s) = %q, want %q", elapsed, got, want)
		}
	}
}

func TestFormatBuildComplete(t *testing.T) {
	want := "#### build completed successfully (02:11:04 (hh:mm:ss)) ####"
	if got := formatBuildComplete(2*time.Hour + 11*time.Minute + 4*time.Second); got != want {
		t.Fatalf("formatBuildComplete() = %q, want %q", got, want)
	}
}

func TestBuildSummary(t *testing.T) {
	var summary buildSummary
	summary.add(SegmentSample{MinimumAvailable: 12 * gibibyte, SwapOutBytes: uint64(gibibyte)})
	summary.add(SegmentSample{MinimumAvailable: 7 * gibibyte, SwapOutBytes: 2 * uint64(gibibyte)})
	want := "uni: phases=2, min available=7.0 GiB, swap-out=3.0 GiB"
	if got := formatBuildSummary(summary); got != want {
		t.Fatalf("formatBuildSummary() = %q, want %q", got, want)
	}
}

func TestBuildOutput(t *testing.T) {
	state := State{
		ProductOut:    "/src/out/target/product/nabu",
		TargetProduct: "uwu_nabu",
		NinjaArgs:     []string{"otapackage"},
	}
	if got, want := formatBuildOutput(state), "uni: package=/src/out/target/product/nabu/uwu_nabu-ota.zip"; got != want {
		t.Fatalf("formatBuildOutput() = %q, want %q", got, want)
	}

	state.NinjaArgs = []string{"SystemUI"}
	if got, want := formatBuildOutput(state), "uni: output=/src/out/target/product/nabu"; got != want {
		t.Fatalf("formatBuildOutput() = %q, want %q", got, want)
	}
}

func TestScheduledR8PrimeTargets(t *testing.T) {
	packages := []string{"r1", "a", "r2", "b"}
	r8Modules := map[string]struct{}{"r1": {}, "r2": {}}
	if got := scheduledR8PrimeTargets(packages, r8Modules, 18, len(packages)); got != nil {
		t.Fatalf("single segment returned R8 prime targets: %v", got)
	}
	if got := scheduledR8PrimeTargets(packages, r8Modules, 18, 2); !reflect.DeepEqual(got, []string{"r1"}) {
		t.Fatalf("multi-segment R8 prime targets: %v", got)
	}
}

func TestUseSingleGraph(t *testing.T) {
	if !useSingleGraph([]string{"a", "b"}, 2) {
		t.Fatal("one batch did not use the final graph fast path")
	}
	if useSingleGraph([]string{"a", "b", "c"}, 2) {
		t.Fatal("multiple batches used the final graph fast path")
	}
}
