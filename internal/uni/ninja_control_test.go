// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWithParallelismFilePreservesExecutorArguments(t *testing.T) {
	for _, test := range []struct {
		name      string
		args, env []string
		want      string
	}{
		{"inherited", []string{"soong_ui", "-j18", "-k0"}, []string{"NINJA_EXTRA_ARGS=-l 12 -v"}, "-l 12 -v"},
		{"command line", []string{"soong_ui", "NINJA_EXTRA_ARGS=-w dupbuild=err"}, []string{"NINJA_EXTRA_ARGS=-l 5"}, "-w dupbuild=err"},
		{"last assignment", []string{"soong_ui", "NINJA_EXTRA_ARGS=-v", "NINJA_EXTRA_ARGS=-d explain"}, nil, "-d explain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.args...)
			args, extra := withParallelismFile(test.args, test.env, "/tmp/jobs")
			if extra != test.want+" --parallelism-file /tmp/jobs" {
				t.Fatalf("extra arguments = %q", extra)
			}
			if !reflect.DeepEqual(original, test.args) {
				t.Fatal("mutated caller's argument slice")
			}
			for index, arg := range args {
				if strings.HasPrefix(arg, "NINJA_EXTRA_ARGS=") {
					if arg != "NINJA_EXTRA_ARGS="+extra {
						t.Fatal(arg)
					}
				} else if arg != original[index] {
					t.Fatal("changed an unrelated argument")
				}
			}
		})
	}
}

func TestParallelismFileRejectsInvalidLimits(t *testing.T) {
	directory := t.TempDir()
	control := &ninjaParallelismFile{path: filepath.Join(directory, "jobs"), ceiling: 18}
	for _, limit := range []int{18, 1, 14} {
		if _, err := control.setParallelism(context.Background(), limit); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{-1, 0, 19} {
		if _, err := control.setParallelism(context.Background(), limit); err == nil {
			t.Fatalf("accepted %d", limit)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := control.setParallelism(ctx, 2); err == nil {
		t.Fatal("accepted canceled request")
	}
	contents, err := os.ReadFile(control.path)
	if err != nil || string(contents) != "14\n" {
		t.Fatalf("last limit changed: %q %v", contents, err)
	}
}

// These are real executor processes and real concurrent shell tasks, not mocks.
// A lower limit must let existing commands finish without admitting replacements.
func checkRuntimeAdmission(t *testing.T, binary string) {
	t.Helper()
	workspace := t.TempDir()
	control, err := prepareNinjaParallelismFile(binary, 3)
	if err != nil || control == nil {
		t.Fatalf("runtime admission unavailable: %v", err)
	}
	defer control.close()
	build := "rule wait\n  command = touch $out.started; while [ ! -f $out.release ]; do sleep 0.02; done; touch $out\n"
	for index := 0; index < 6; index++ {
		build += fmt.Sprintf("build job%d: wait\n", index)
	}
	if err := os.WriteFile(filepath.Join(workspace, "build.ninja"), []byte(build), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-j3", "--parallelism-file", control.path)
	command.Dir = workspace
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		for index := 0; index < 6; index++ {
			_ = os.WriteFile(filepath.Join(workspace, fmt.Sprintf("job%d.release", index)), nil, 0644)
		}
		cancel()
	}()
	started := func() []int {
		var result []int
		for index := 0; index < 6; index++ {
			if _, err := os.Stat(filepath.Join(workspace, fmt.Sprintf("job%d.started", index))); err == nil {
				result = append(result, index)
			}
		}
		return result
	}
	waitCount := func(want int) []int {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if jobs := started(); len(jobs) == want {
				return jobs
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("started jobs = %v, want %d", started(), want)
		return nil
	}
	release := func(job int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("job%d.release", job)), nil, 0644); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(workspace, fmt.Sprintf("job%d", job))); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("running job%d was interrupted", job)
	}
	initial := waitCount(3)
	if _, err := control.setParallelism(ctx, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	release(initial[0])
	time.Sleep(150 * time.Millisecond)
	if len(started()) != 3 {
		t.Fatal("admitted replacement while above reduced limit")
	}
	// Invalid or missing updates retain the last valid limit, including a value
	// exceeding the command-line ceiling. They must never stop the executor.
	if err := os.WriteFile(control.path, []byte("4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	release(initial[1])
	time.Sleep(150 * time.Millisecond)
	if len(started()) != 3 {
		t.Fatal("invalid limit replaced the last valid limit")
	}
	if err := os.Remove(control.path); err != nil {
		t.Fatal(err)
	}
	release(initial[2])
	waitCount(4)
	time.Sleep(150 * time.Millisecond)
	if len(started()) != 4 {
		t.Fatal("missing control file reset admission")
	}
	if _, err := control.setParallelism(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Admission is re-evaluated at the next scheduler turn (task completion).
	for _, job := range started() {
		if job != initial[0] && job != initial[1] && job != initial[2] {
			release(job)
		}
	}
	waitCount(6)
	for _, job := range started() {
		release(job)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("executor did not finish")
	}
}

func checkExecutorOptions(t *testing.T, binary string) {
	t.Helper()
	workspace := t.TempDir()
	build := "rule fail\n  command = false\nrule succeed\n  command = touch $out\npool serial\n  depth = 1\nrule pooled\n  command = touch $out\n  pool = serial\nbuild fail: fail\nbuild success: succeed\nbuild pool1: pooled\nbuild pool2: pooled\nbuild group: phony success pool1 pool2\nbuild weighted: succeed\n"
	if err := os.WriteFile(filepath.Join(workspace, "build.ninja"), []byte(build), 0644); err != nil {
		t.Fatal(err)
	}
	// These are Soong's actual Ninja flags plus Uni's -k, -l, showcommands.
	command := exec.Command(binary, "-j2", "-k0", "-l999999", "-v", "-d", "keepdepfile", "-d", "keeprsp", "-d", "stats", "-o", "usesphonyoutputs=yes", "-w", "dupbuild=err", "-w", "missingdepfile=err", "-w", "missingoutfile=err", "fail", "group")
	command.Dir = workspace
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "touch success") {
		t.Fatalf("keep-going/verbose flags failed: %v\n%s", err, output)
	}
	for _, name := range []string{"success", "pool1", "pool2"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("-k0 did not finish %s: %v\n%s", name, err, output)
		}
	}
	weights := filepath.Join(workspace, "weights")
	frontend := filepath.Join(workspace, "status.pb")
	if err := os.WriteFile(weights, []byte("weighted,100\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(binary, "-j2", "--frontend_file", frontend, "-o", "usesweightlist="+weights, "weighted")
	command.Dir = workspace
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Soong frontend/weight-list flags failed: %v\n%s", err, output)
	}
	if info, err := os.Stat(frontend); err != nil || info.Size() == 0 {
		t.Fatalf("no frontend events: %v", err)
	}
}

// Exercise Uni's environment/wrapper selection, the path that caused the
// missing Runa socket when --assume-existing selected a different executor.
func checkAssumeExistingRunner(t *testing.T, top, binary string) {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "soong_ui.bash")
	contents := `#!/bin/sh
set -eu
for arg do
  case "$arg" in NINJA_EXTRA_ARGS=*) NINJA_EXTRA_ARGS=${arg#NINJA_EXTRA_ARGS=} ;; esac
done
printf '%s\n' "$UNI_NINJA_BIN" > executor
printf '%s\n' "$NINJA_EXTRA_ARGS" > extra
test "$UNI_ASSUME_EXISTING" = true
# Soong similarly splits Ninja's extra arguments into executor argv.
exec "$UNI_NINJA_BIN" -j2 -d assumeexisting $NINJA_EXTRA_ARGS
`
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "build.ninja"), []byte("rule generate\n  command = touch output\nbuild output: generate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runner := &commandRunner{
		top: directory, outDir: directory, soongUIPath: script,
		baseEnv:        overrideEnvironment(os.Environ(), "NINJA_EXTRA_ARGS=-d explain"),
		requestedNinja: "runa", phasedNinja: "runa", assumeExistingNinja: binary,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := runner.runReported(ctx, nil, &buildSummary{}, "test", "--uni-ninja-mode", "only", filepath.Join(directory, "state.json"), []string{"-j2", "NINJA_EXTRA_ARGS=-v"}, 2); err != nil {
		t.Fatal(err)
	}
	executor, err := os.ReadFile(filepath.Join(directory, "executor"))
	if err != nil {
		t.Fatal(err)
	}
	help, _ := exec.Command(binary, "--help").CombinedOutput()
	if strings.Contains(string(help), "--control-socket") {
		if !strings.Contains(string(executor), "ninja-wrapper") {
			t.Fatalf("Runa wrapper was overwritten: %s", executor)
		}
	} else if strings.TrimSpace(string(executor)) != binary {
		t.Fatalf("unexpected native executor: %s", executor)
	}
	extra, err := os.ReadFile(filepath.Join(directory, "extra"))
	if err != nil || !strings.HasPrefix(string(extra), "-v --parallelism-file ") {
		t.Fatalf("command-line arguments discarded admission control: %s %v", extra, err)
	}
}

func checkNormalNinjaRunner(t *testing.T, top, outDir, binary string) {
	t.Helper()
	directory := t.TempDir()
	for relative, target := range map[string]string{
		"external/ninja": filepath.Join(top, "external/ninja"),
		"prebuilts/build-tools/linux-x86/bin/ninja": filepath.Join(top, "prebuilts/build-tools/linux-x86/bin/ninja"),
	} {
		path := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(directory, "soong_ui.bash")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
test -z "${UNI_ASSUME_EXISTING:-}"
printf '%s\n' "$UNI_NINJA_BIN" > executor
exec "$UNI_NINJA_BIN" -j2 $NINJA_EXTRA_ARGS
`), 0700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"build.ninja": "rule copy\n  command = cp input output\nbuild output: copy input\n",
		"input":       "new content", "output": "unlogged output",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &commandRunner{top: directory, outDir: outDir, soongUIPath: script,
		baseEnv: overrideEnvironment(os.Environ(), "UNI_ASSUME_EXISTING=", "UNI_NINJA_BIN="), requestedNinja: "ninja", phasedNinja: "ninja"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := runner.runReported(ctx, nil, &buildSummary{}, "test", "--uni-ninja-mode", "only", filepath.Join(outDir, "state.json"), []string{"-j2"}, 2); err != nil {
		t.Fatal(err)
	}
	executor, err := os.ReadFile(filepath.Join(directory, "executor"))
	if err != nil || strings.TrimSpace(string(executor)) != binary {
		t.Fatalf("normal Ninja bypassed compatibility executor: %s %v", executor, err)
	}
	contents, err := os.ReadFile(filepath.Join(directory, "output"))
	if err != nil || string(contents) != "new content" {
		t.Fatalf("normal Ninja incorrectly enabled assumeexisting: %s %v", contents, err)
	}
}
