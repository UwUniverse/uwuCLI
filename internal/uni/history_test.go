// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateDurationHistoryUsesTaskMetadata(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, ".ninja_log")
	data := "# ninja log v5\n0\t1250\t1\tout/app.jar\thash\n0\t1250\t1\tout/app.apk\thash\n"
	if err := os.WriteFile(logPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	history := newBuildHistory("uwu_test", "source", "toolchain")
	metadata := taskMetadata{Version: 1, Actions: []taskAction{{
		Module: "App", Rule: "r8", TaskType: "r8",
		Outputs: []string{"out/app.jar", "out/app.apk"},
	}}}
	if err := updateDurationHistory(&history, logPath, metadata); err != nil {
		t.Fatal(err)
	}
	if got := history.Modules["App"].DurationMS.EWMA; got != 1250 {
		t.Fatalf("module duration = %.0f, want 1250", got)
	}
	if got := history.TaskTypes["r8"].DurationMS.EWMA; got != 1250 {
		t.Fatalf("task duration = %.0f, want 1250", got)
	}
}

func TestInterleaveLongTargetsIsDeterministic(t *testing.T) {
	targets := []string{"a", "b", "c", "d", "e", "f"}
	weights := map[string]float64{"a": 10, "b": 20}
	first := InterleaveLongTargets(targets, weights)
	second := InterleaveLongTargets(targets, weights)
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, []string{"c", "b", "d", "e", "a", "f"}) {
		t.Fatalf("unexpected long-target order: %v", first)
	}
}

func TestLongPrimeTargetsUsesAccumulatedDuration(t *testing.T) {
	targets := []string{"a", "b", "c", "d"}
	weights := map[string]float64{"a": 10, "b": 80, "c": 40}
	if got, want := LongPrimeTargets(targets, weights, 2), []string{"b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("long prime targets = %v, want %v", got, want)
	}
}

func TestHistoryWeightsUsesOnePersistentSampleAcrossSourceChanges(t *testing.T) {
	top := t.TempDir()
	history := newBuildHistory("uwu_test", "old-source", "old-toolchain")
	entry := &moduleHistory{}
	entry.DurationMS.observe(4200)
	history.Modules["SlowModule"] = entry
	history.BuildSamples = 1
	if err := saveBuildHistory(historyPath(top, "uwu_test"), history); err != nil {
		t.Fatal(err)
	}
	weights, source := historyWeights(top, State{TargetProduct: "uwu_test", SourceFingerprint: "new-source"}, nil)
	if weights["SlowModule"] != 4200 || !strings.Contains(source, "persistent samples=1") {
		t.Fatalf("weights=%v source=%q", weights, source)
	}
}

func TestHistoryWeightsFallsBackToNinjaLog(t *testing.T) {
	top := t.TempDir()
	outDir := filepath.Join(top, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_log"), []byte("# ninja log v5\n0\t2300\t1\tout/slow.jar\thash\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(outDir, "task_metadata.json")
	metadata := taskMetadata{Version: 1, Actions: []taskAction{{
		Module: "SlowModule", Rule: "javac", TaskType: "javac", Outputs: []string{"out/slow.jar"},
	}}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	weights, source := historyWeights(top, State{
		TargetProduct: "uwu_test", OutDir: outDir, TaskMetadata: metadataPath,
	}, nil)
	if weights["SlowModule"] != 2300 || !strings.Contains(source, "ninja-log") {
		t.Fatalf("weights=%v source=%q", weights, source)
	}
}

func TestHistoryWeightsFallsBackToSoongHint(t *testing.T) {
	top := t.TempDir()
	outDir := filepath.Join(top, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_weight_list"), []byte("out/slow.jar,8400\nout/fast.jar,200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(outDir, "task_metadata.json")
	metadata := taskMetadata{Version: 1, Actions: []taskAction{
		{Module: "SlowModule", Rule: "kotlinc", TaskType: "kotlinc", Outputs: []string{"out/slow.jar"}},
		{Module: "FastModule", Rule: "javac", TaskType: "javac", Outputs: []string{"out/fast.jar"}},
	}}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	weights, source := historyWeights(top, State{
		TargetProduct: "uwu_test", OutDir: outDir, TaskMetadata: metadataPath,
	}, nil)
	if weights["SlowModule"] != 8400 || weights["FastModule"] != 200 || !strings.Contains(source, "soong-hint") {
		t.Fatalf("weights=%v source=%q", weights, source)
	}
}

func TestBuildHistoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	history := newBuildHistory("uwu_test", "source", "toolchain")
	history.BuildSamples = 3
	if err := saveBuildHistory(path, history); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadBuildHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BuildSamples != 3 || loaded.SourceFingerprint != "source" {
		t.Fatalf("unexpected history: %+v", loaded)
	}
}
