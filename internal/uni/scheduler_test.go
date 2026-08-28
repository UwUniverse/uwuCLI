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

func TestFinalNinjaTargetsIncludesKernelWithoutExtraPhase(t *testing.T) {
	state := State{NinjaArgs: []string{"otapackage"}}
	want := []string{"kernel", "otapackage"}
	if got := finalNinjaTargets(state, Options{}, "kernel"); !reflect.DeepEqual(got, want) {
		t.Fatalf("final targets = %v, want %v", got, want)
	}
	state.NinjaArgs = []string{"kernel", "otapackage"}
	if got := finalNinjaTargets(state, Options{}, "kernel"); !reflect.DeepEqual(got, state.NinjaArgs) {
		t.Fatalf("kernel target was duplicated: %v", got)
	}
}

func TestCleanBuildThroughputDefaults(t *testing.T) {
	options, err := ParseOptions([]string{"-j18", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := MemorySnapshot{
		Total: 32 * gibibyte, Available: 28 * gibibyte,
		SwapTotal: 50 * gibibyte, SwapFree: 48 * gibibyte,
	}
	batch := scheduledBatchSize(options, 1528, snapshot)
	if batch != 1528 || !useSingleGraph(make([]string, 1528), batch) {
		t.Fatalf("batch=%d did not select one full graph", batch)
	}
	decision := (&poolAdmission{}).decide(options.MaxJobs, snapshot, nil, 4)
	if decision.highmem != 18 || decision.r8 != 18 || decision.rust != 5 || decision.java != 12 || decision.kotlin != 6 {
		t.Fatalf("throughput pools = %+v, want 18/18/5/12/6", decision)
	}
	if got := phaseArgs(options, []string{"otapackage"}, 18, false); !reflect.DeepEqual(got, []string{"-j18", "otapackage"}) {
		t.Fatalf("phase args = %v", got)
	}
	if got := startupPackageLimit(18, 1528); got != 9 {
		t.Fatalf("startup package limit = %d, want 9", got)
	}
	if got := startupPhaseJobs(18, 18, true); got != 1 {
		t.Fatalf("startup phase jobs = %d, want 1", got)
	}
	if got := startupPhaseJobs(18, 18, false); got != 18 {
		t.Fatalf("kernel-free startup phase jobs = %d, want 18", got)
	}
}

func TestSelectStartupSchedulePrioritizesHistoricalLongTargets(t *testing.T) {
	packages := []string{"r1", "a", "r2", "b", "r3", "c"}
	r8Modules := map[string]struct{}{"r1": {}, "r2": {}, "r3": {}}
	weights := map[string]float64{"a": 90, "b": 80, "r1": 70}
	schedule := selectStartupSchedule(packages, weights, r8Modules, "kernel", 8)
	want := []string{"kernel", "a", "b", "r1", "r2"}
	if !reflect.DeepEqual(schedule.targets, want) {
		t.Fatalf("startup targets = %v, want %v", schedule.targets, want)
	}
	if schedule.packageCount != 4 || schedule.historyCount != 3 || schedule.r8Count != 2 || schedule.nonR8Count != 2 {
		t.Fatalf("unexpected startup counts: %+v", schedule)
	}
	if got := startupPackageLimit(1, len(packages)); got != 4 {
		t.Fatalf("small-machine startup limit = %d, want 4", got)
	}
}

func TestSingleGraphStartupKeepsKernelExclusive(t *testing.T) {
	schedule := startupSchedule{
		targets:      []string{"kernel", "Settings", "SystemUI"},
		packageCount: 2, r8Count: 2,
	}
	got := constrainStartupForGraph(schedule, "kernel", true)
	if !reflect.DeepEqual(got.targets, []string{"kernel"}) || got.packageCount != 0 || got.r8Count != 0 {
		t.Fatalf("single-graph startup = %+v, want exclusive kernel", got)
	}
	if got := constrainStartupForGraph(schedule, "kernel", false); !reflect.DeepEqual(got, schedule) {
		t.Fatalf("multi-graph startup changed: %+v", got)
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

func TestTakeBatchWithR8Limit(t *testing.T) {
	targets := []string{"r1", "r2", "r3", "a", "b", "c"}
	r8 := map[string]struct{}{"r1": {}, "r2": {}, "r3": {}}
	batch := takeBatchWithR8Limit(targets, 4, r8, 1)
	if got := R8TargetCount(batch, r8); got != 1 {
		t.Fatalf("R8 count = %d, want 1: %v", got, batch)
	}
	if len(batch) != 4 {
		t.Fatalf("batch size = %d, want 4: %v", len(batch), batch)
	}
}
