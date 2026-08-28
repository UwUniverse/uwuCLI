// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadSoongStateProtocolFixture(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	fixture := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "build", "soong", "ui", "build", "testdata", "uni_state_v7.json")
	state, err := LoadState(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != stateVersion {
		t.Fatalf("Soong state version = %d, want %d", state.Version, stateVersion)
	}
	if !state.SkipKatiNinja {
		t.Fatal("CLI lost Soong SkipKatiNinja")
	}
	if state.TaskMetadata == "" {
		t.Fatal("CLI lost Soong task metadata path")
	}
	roundTrip := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(roundTrip, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.SkipKatiNinja || loaded.TaskMetadata != state.TaskMetadata {
		t.Fatal("CLI state round trip lost protocol fields")
	}
}

func TestStateValidationRejectsOldProtocol(t *testing.T) {
	state := State{Version: stateVersion - 1}
	if err := state.Validate("", "", ""); err == nil || !strings.Contains(err.Error(), "unsupported state version 6") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestProductTargetsAndStableShuffle(t *testing.T) {
	state := State{
		ProductPackages: []string{"a", "missing", "b", "a", "c"},
		AllModules:      []string{"a", "b", "c"},
	}
	targets := ProductTargets(state)
	if !reflect.DeepEqual(targets, []string{"a", "b", "c"}) {
		t.Fatalf("unexpected targets: %v", targets)
	}
	first := StableShuffle(targets, "uwu_nabu")
	second := StableShuffle(targets, "uwu_nabu")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("shuffle is not stable: %v != %v", first, second)
	}
}

func TestBatches(t *testing.T) {
	targets := make([]string, 1001)
	for i := range targets {
		targets[i] = string(rune(i))
	}
	batches := Batches(targets, 500)
	if len(batches) != 3 || len(batches[0]) != 500 || len(batches[1]) != 500 || len(batches[2]) != 1 {
		t.Fatalf("unexpected batch sizes")
	}
}

func TestDetectR8Modules(t *testing.T) {
	directory := t.TempDir()
	combined := filepath.Join(directory, "combined.ninja")
	soongDirectory := filepath.Join(directory, "out", "soong")
	if err := os.MkdirAll(soongDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(soongDirectory, "build.test.ninja")
	shard := filepath.Join(soongDirectory, "build.test.0.ninja")
	if err := os.WriteFile(combined, []byte("subninja out/soong/build.test.ninja\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("subninja out/soong/build.test.0.ninja\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := `build out/app.jar: g.java.r8 in/app.jar
    tags = module_name=App;module_type=android_app;rule_name=r8
build out/lib.jar: g.java.d8 in/lib.jar
    tags = module_name=Library;module_type=java_library;rule_name=d8
build out/partial.jar: g.java.d8r8 in/partial.jar
    tags = module_name=PartialApp;module_type=android_app;rule_name=d8r8
`
	if err := os.WriteFile(shard, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	modules, err := DetectR8Modules(State{SourceRoot: directory, CombinedNinja: combined})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := modules["App"]; !ok {
		t.Fatalf("R8 module was not detected: %v", modules)
	}
	if _, ok := modules["Library"]; ok {
		t.Fatalf("D8 module was detected as R8: %v", modules)
	}
	if _, ok := modules["PartialApp"]; !ok {
		t.Fatalf("combined D8/R8 module was not detected: %v", modules)
	}
}

func TestLoadR8ModulesCachesAndInvalidates(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "build.ninja")
	cache := filepath.Join(directory, "r8_modules.json")
	if err := os.WriteFile(root, []byte("    tags = module_name=App;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := State{SourceRoot: directory, SoongNinja: root}
	modules, err := LoadR8Modules(state, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := modules["App"]; !ok {
		t.Fatalf("R8 module was not cached: %v", modules)
	}
	if err := os.WriteFile(root, []byte("    tags = module_name=Other;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	modules, err = LoadR8Modules(state, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := modules["Other"]; !ok {
		t.Fatalf("changed R8 module was not detected: %v", modules)
	}
}

func TestLoadR8ModulesUsesPreparedList(t *testing.T) {
	modules, err := LoadR8Modules(State{R8Modules: []string{"App", "SystemUI"}, R8ModulesReady: true}, filepath.Join(t.TempDir(), "cache"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := modules["SystemUI"]; !ok || len(modules) != 2 {
		t.Fatalf("prepared R8 modules were not used: %v", modules)
	}
}

func TestDevR8ScanCreatesCacheForFastRuns(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "build.ninja")
	cache := filepath.Join(directory, "r8_modules.json")
	if err := os.WriteFile(root, []byte("    tags = module_name=FullApp;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := State{
		SourceRoot:     directory,
		SoongNinja:     root,
		R8Modules:      []string{"FastApp"},
		R8ModulesReady: true,
	}
	modules, source, err := LoadR8ModulesDetailed(state, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if source != "ninja-full-scan" {
		t.Fatalf("unexpected developer scan source: %s", source)
	}
	if _, ok := modules["FullApp"]; !ok {
		t.Fatalf("complete Ninja scan was not used: %v", modules)
	}
	modules, source, err = LoadR8ModulesDetailed(state, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "full-scan-cache" {
		t.Fatalf("complete scan cache was not reused: %s", source)
	}
	if _, ok := modules["FullApp"]; !ok {
		t.Fatalf("cached complete scan was not used: %v", modules)
	}
}

func TestR8IndexModes(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "build.ninja")
	cache := filepath.Join(directory, "r8_modules.json")
	if err := os.WriteFile(root, []byte("    tags = module_name=First;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := State{SourceRoot: directory, SoongNinja: root}
	modules, source, err := LoadR8ModulesForModeContext(context.Background(), state, cache, R8IndexAuto)
	if err != nil {
		t.Fatal(err)
	}
	if source != "ninja-full-scan" {
		t.Fatalf("automatic mode did not create an index: %s", source)
	}
	if _, ok := modules["First"]; !ok {
		t.Fatalf("automatic mode missed R8 module: %v", modules)
	}
	modules, source, err = LoadR8ModulesForModeContext(context.Background(), state, cache, R8IndexAuto)
	if err != nil {
		t.Fatal(err)
	}
	if source != "full-scan-cache" {
		t.Fatalf("automatic mode did not reuse the index: %s", source)
	}
	if err := os.WriteFile(root, []byte("    tags = module_name=Second;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	modules, source, err = LoadR8ModulesForModeContext(context.Background(), state, cache, R8IndexAuto)
	if err != nil {
		t.Fatal(err)
	}
	if source != "ninja-full-scan" {
		t.Fatalf("automatic mode reused a stale index: %s", source)
	}
	if _, ok := modules["Second"]; !ok {
		t.Fatalf("automatic mode did not refresh the index: %v", modules)
	}
	if err := os.WriteFile(root, []byte("    tags = module_name=Forced;rule_name=r8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	modules, source, err = LoadR8ModulesForModeContext(context.Background(), state, cache, R8IndexFull)
	if err != nil {
		t.Fatal(err)
	}
	if source != "ninja-full-scan" {
		t.Fatalf("full mode reused the index: %s", source)
	}
	if _, ok := modules["Forced"]; !ok {
		t.Fatalf("full mode did not refresh the index: %v", modules)
	}
}

func TestFastR8ScanDoesNotReadNinja(t *testing.T) {
	directory := t.TempDir()
	cache := filepath.Join(directory, "r8_modules.json")
	state := State{
		SourceRoot:     directory,
		SoongNinja:     filepath.Join(directory, "missing.ninja"),
		R8Modules:      []string{"FastApp"},
		R8ModulesReady: true,
	}
	modules, source, err := LoadR8ModulesDetailed(state, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	if source != "soong-fast" {
		t.Fatalf("unexpected fast scan source: %s", source)
	}
	if _, ok := modules["FastApp"]; !ok {
		t.Fatalf("prepared R8 list was not used: %v", modules)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("fast mode created a complete scan cache: %v", err)
	}
}

func TestR8ScanHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := LoadR8ModulesDetailedContext(ctx, State{}, filepath.Join(t.TempDir(), "cache"), true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context canceled", err)
	}
}

func TestInterleaveR8Targets(t *testing.T) {
	targets := []string{"r1", "r2", "r3", "r4", "a", "b", "c", "d", "e", "f", "g", "h"}
	r8 := map[string]struct{}{"r1": {}, "r2": {}, "r3": {}, "r4": {}}
	ordered := InterleaveR8Targets(targets, r8)
	if len(ordered) != len(targets) {
		t.Fatalf("unexpected target count: %v", ordered)
	}
	for start := 0; start < len(ordered); start += 3 {
		end := min(start+3, len(ordered))
		if R8TargetCount(ordered[start:end], r8) == 0 {
			t.Fatalf("R8 target missing from interval: %v", ordered)
		}
	}
}

func TestR8PrimeTargetsKeepsLaterWork(t *testing.T) {
	targets := []string{"r1", "a", "r2", "b", "r3", "c", "r4", "d", "r5", "e", "r6", "f"}
	r8 := map[string]struct{}{"r1": {}, "r2": {}, "r3": {}, "r4": {}, "r5": {}, "r6": {}}
	prime := R8PrimeTargets(targets, r8, 18, 2)
	if !reflect.DeepEqual(prime, []string{"r1", "r4"}) {
		t.Fatalf("unexpected prime targets: %v", prime)
	}
}

func TestR8TargetsRemainAcrossProductSegments(t *testing.T) {
	var targets []string
	r8 := make(map[string]struct{})
	for index := 0; index < 77; index++ {
		name := "r" + strconv.Itoa(index)
		targets = append(targets, name)
		r8[name] = struct{}{}
	}
	for index := 0; index < 1451; index++ {
		targets = append(targets, "n"+strconv.Itoa(index))
	}
	ordered := InterleaveR8Targets(targets, r8)
	prime := R8PrimeTargets(ordered, r8, 18, 2)
	completed := make(map[string]struct{}, len(prime))
	for _, target := range prime {
		completed[target] = struct{}{}
	}
	remaining := removeCompleted(append([]string(nil), ordered...), completed)
	if R8TargetCount(remaining[:1000], r8) == 0 || R8TargetCount(remaining[1000:], r8) == 0 {
		t.Fatalf("R8 targets were not retained across segments")
	}
}

func TestStateValidationDetectsGraphChange(t *testing.T) {
	directory := t.TempDir()
	graph := filepath.Join(directory, "combined.ninja")
	if err := os.WriteFile(graph, []byte("ninja"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(graph)
	if err != nil {
		t.Fatal(err)
	}
	files := []GraphFile{{Path: graph, Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}}
	fingerprint, err := graphFingerprint(files)
	if err != nil {
		t.Fatal(err)
	}
	state := State{Version: stateVersion, SourceRoot: directory, OutDir: directory,
		TargetProduct: "uwu_nabu", BuildDateTime: "1", BuildDateTimeFile: graph,
		GraphFiles: files, GraphFingerprint: fingerprint}
	if err := state.Validate(directory, directory, "uwu_nabu"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, []byte("changed ninja"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := state.Validate(directory, directory, "uwu_nabu"); err == nil {
		t.Fatal("changed graph was accepted")
	}
}

func TestReuseStateChangesTargetAndRestoresBuildDate(t *testing.T) {
	directory := t.TempDir()
	outDir := filepath.Join(directory, "out")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "Android.bp")
	graph := filepath.Join(outDir, "combined.ninja")
	buildDate := filepath.Join(outDir, "build_date.txt")
	if err := os.WriteFile(config, []byte("filegroup {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, []byte("build droid: phony\nbuild otapackage: phony\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildDate, []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(config, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	files := make([]GraphFile, 0, 2)
	for _, path := range []string{graph, buildDate} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, GraphFile{Path: path, Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()})
	}
	fingerprint, err := graphFingerprint(files)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(outDir, "state.json")
	state := State{Version: stateVersion, SourceRoot: directory, OutDir: outDir,
		TargetProduct: "uwu_test", TargetRelease: "cp2a", BuildVariant: "userdebug",
		BuildDateTime: "123", BuildDateTimeFile: buildDate, GraphFiles: files,
		GraphFingerprint: fingerprint, NinjaArgs: []string{"droid"}}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	reusedState, reused, err := ReuseState(statePath, directory, outDir, "uwu_test", "cp2a", "userdebug",
		Options{Targets: []string{"otapackage"}, BuildArgs: []string{"otapackage"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || !reflect.DeepEqual(reusedState.NinjaArgs, []string{"otapackage"}) {
		t.Fatalf("state was not reused: reused=%t targets=%v", reused, reusedState.NinjaArgs)
	}
	data, err := os.ReadFile(buildDate)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "123\n" {
		t.Fatalf("build date was not restored: %q", data)
	}
}

func TestReuseStateRejectsGraphSourceChange(t *testing.T) {
	directory := t.TempDir()
	outDir := filepath.Join(directory, "out")
	if err := os.MkdirAll(outDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "Android.bp")
	graph := filepath.Join(outDir, "combined.ninja")
	if err := os.WriteFile(config, []byte("filegroup {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, []byte("build droid: phony\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(graph)
	if err != nil {
		t.Fatal(err)
	}
	files := []GraphFile{{Path: graph, Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}}
	fingerprint, err := graphFingerprint(files)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(outDir, "state.json")
	state := State{Version: stateVersion, SourceRoot: directory, OutDir: outDir,
		TargetProduct: "uwu_test", TargetRelease: "cp2a", BuildVariant: "userdebug",
		BuildDateTime: "123", BuildDateTimeFile: filepath.Join(outDir, "date"),
		GraphFiles: files, GraphFingerprint: fingerprint, SourceFingerprint: "stale"}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	_, reused, err := ReuseState(statePath, directory, outDir, "uwu_test", "cp2a", "userdebug", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("changed source graph was reused")
	}
}

func TestNinjaTarget(t *testing.T) {
	directory := t.TempDir()
	kati := filepath.Join(directory, "build.ninja")
	if err := os.WriteFile(kati, []byte("build foo: phony\nbuild kernel: phony out/kernel\n"), 0600); err != nil {
		t.Fatal(err)
	}
	found, err := HasNinjaTarget(kati, "kernel")
	if err != nil || !found {
		t.Fatalf("kernel target not found: %v", err)
	}
}

func TestEarlyKernelTarget(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		goal     string
		priority string
	}{
		{name: "source kernel", data: "build bootimage: phony out/boot.img\nbuild kernel: phony out/kernel | implicit || order\n", goal: "kernel", priority: "out/kernel"},
		{name: "GKI boot image", data: "build bootimage: phony out/boot.img\n", goal: "bootimage", priority: "out/boot.img"},
		{name: "real kernel target", data: "build kernel: kernel_rule source\n", goal: "kernel", priority: "kernel"},
		{name: "no early target", data: "build droid: phony out/system.img\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "build.ninja")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			goal, priority, err := EarlyKernelTargets(path)
			if err != nil {
				t.Fatal(err)
			}
			if goal != test.goal || priority != test.priority {
				t.Fatalf("got goal=%q priority=%q, want goal=%q priority=%q", goal, priority, test.goal, test.priority)
			}
			legacyGoal, err := EarlyKernelTarget(path)
			if err != nil || legacyGoal != test.goal {
				t.Fatalf("EarlyKernelTarget() = %q, %v", legacyGoal, err)
			}
		})
	}
}
