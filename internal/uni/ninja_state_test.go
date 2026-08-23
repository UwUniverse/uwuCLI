// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testNinjaLog(entries ...string) string {
	return ninjaLogHeader + "\n" + strings.Join(entries, "\n") + "\n"
}

func testNinjaDeps(payload string) []byte {
	data := append([]byte(nil), []byte(ninjaDepsHeader)...)
	version := make([]byte, 4)
	binary.LittleEndian.PutUint32(version, ninjaDepsVersion)
	data = append(data, version...)
	return append(data, payload...)
}

func TestMergeNinjaLogKeepsLatestEntries(t *testing.T) {
	outDir := t.TempDir()
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_log")
	if err := os.MkdirAll(filepath.Dir(backupPath), 0755); err != nil {
		t.Fatal(err)
	}
	backup := testNinjaLog(
		"1\t2\t3\tout/a\told-a",
		"2\t3\t4\tout/b\told-b",
	)
	current := testNinjaLog(
		"3\t4\t5\tout/b\tnew-b",
		"4\t5\t6\tout/c\tnew-c",
	)
	if err := os.WriteFile(backupPath, []byte(backup), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_log"), []byte(current), 0644); err != nil {
		t.Fatal(err)
	}
	older, err := readNinjaLog(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := readNinjaLog(filepath.Join(outDir, ".ninja_log"))
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeNinjaLogs(older, newer)
	if len(merged.lines) != 3 {
		t.Fatalf("got %d entries, want 3", len(merged.lines))
	}
	if got := merged.lines["out/b"]; !strings.HasSuffix(got, "\tnew-b") {
		t.Fatalf("current entry did not override backup: %q", got)
	}
}

func TestRecoverNinjaLogRestoresTruncatedCurrent(t *testing.T) {
	outDir := t.TempDir()
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_log")
	if err := os.MkdirAll(filepath.Dir(backupPath), 0755); err != nil {
		t.Fatal(err)
	}
	entries := make([]string, 0, 2)
	for index, name := range []string{"a", "b"} {
		path := filepath.Join(outDir, name)
		if err := os.WriteFile(path, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf("%d\t%d\t%d\t%s\thash-%s",
			index, index+1, info.ModTime().UnixNano(), name, name))
	}
	backup := testNinjaLog(entries...)
	if err := os.WriteFile(backupPath, []byte(backup), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_log"), []byte(ninjaLogHeader+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}
	recovered, err := readNinjaLog(filepath.Join(outDir, ".ninja_log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.lines) != 2 {
		t.Fatalf("got %d entries, want 2", len(recovered.lines))
	}
}

func TestPrepareNinjaStateSurvivesGraphLogReset(t *testing.T) {
	outDir := t.TempDir()
	outputPath := filepath.Join(outDir, "artifact")
	if err := os.WriteFile(outputPath, []byte("complete"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf("1\t2\t%d\tartifact\thash", info.ModTime().UnixNano())
	currentPath := filepath.Join(outDir, ".ninja_log")
	if err := os.WriteFile(currentPath, []byte(testNinjaLog(entry)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte(ninjaLogHeader+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}
	recovered, err := readNinjaLog(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.lines) != 1 {
		t.Fatalf("graph reset recovery retained %d entries, want 1", len(recovered.lines))
	}
}

func TestRecoverNinjaDepsRestoresLargerValidBackup(t *testing.T) {
	outDir := t.TempDir()
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_deps")
	if err := os.MkdirAll(filepath.Dir(backupPath), 0755); err != nil {
		t.Fatal(err)
	}
	backup := testNinjaDeps(strings.Repeat("x", 4096))
	current := testNinjaDeps("short")
	if err := os.WriteFile(backupPath, backup, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_deps"), current, 0644); err != nil {
		t.Fatal(err)
	}
	if err := recoverNinjaDeps(outDir); err != nil {
		t.Fatal(err)
	}
	recovered, err := os.ReadFile(filepath.Join(outDir, ".ninja_deps"))
	if err != nil {
		t.Fatal(err)
	}
	if string(recovered) != string(backup) {
		t.Fatal("Ninja deps backup was not restored")
	}
}

func TestNinjaDepsVersionIsValidated(t *testing.T) {
	outDir := t.TempDir()
	path := filepath.Join(outDir, ".ninja_deps")
	data := testNinjaDeps("")
	binary.LittleEndian.PutUint32(data[len(ninjaDepsHeader):], ninjaDepsVersion+1)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	valid, err := validNinjaDeps(path)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("unsupported Ninja deps version was accepted")
	}
}

func TestCheckpointNinjaDepsSkipsUnchangedSnapshot(t *testing.T) {
	outDir := t.TempDir()
	currentPath := filepath.Join(outDir, ".ninja_deps")
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_deps")
	if err := os.WriteFile(currentPath, testNinjaDeps("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkpointNinjaDeps(outDir); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpointNinjaDeps(outDir); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	firstStat := first.Sys().(*syscall.Stat_t)
	secondStat := second.Sys().(*syscall.Stat_t)
	if firstStat.Ino != secondStat.Ino {
		t.Fatal("unchanged Ninja deps snapshot was rewritten")
	}
}

func TestRecoveredNinjaLogDoesNotAcceptInterruptedOutput(t *testing.T) {
	ninja, err := exec.LookPath("ninja")
	if err != nil {
		t.Skip("ninja is not installed")
	}
	outDir := t.TempDir()
	inputPath := filepath.Join(outDir, "input")
	outputPath := filepath.Join(outDir, "output")
	runsPath := filepath.Join(outDir, "runs")
	buildFile := "rule generate\n" +
		"  command = cp $in $out && printf 'run\\n' >> runs\n" +
		"build output: generate input\n"
	if err := os.WriteFile(filepath.Join(outDir, "build.ninja"), []byte(buildFile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("complete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runNinja := func() {
		command := exec.Command(ninja, "-C", outDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("ninja failed: %v\n%s", err, output)
		}
	}
	runNinja()
	if err := checkpointNinjaState(outDir, false, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("interrupted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(outputPath, future, future); err != nil {
		t.Fatal(err)
	}
	if err := markNinjaRecoveryRequired(outDir); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}
	recoveredLog, err := readNinjaLog(filepath.Join(outDir, ".ninja_log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveredLog.lines) != 0 {
		t.Fatalf("interrupted output retained a Ninja log entry: %v", recoveredLog.lines)
	}
	runNinja()
	if err := checkpointNinjaState(outDir, false, false); err != nil {
		t.Fatal(err)
	}
	if err := clearNinjaRecoveryRequired(outDir); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "complete\n" {
		t.Fatalf("interrupted output was accepted: %q", output)
	}
	runs, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(runs), "run\n") != 2 {
		t.Fatalf("expected recovery rebuild, got %q", runs)
	}
}

func TestTrustedRecoveryKeepsInterruptedOutputProgress(t *testing.T) {
	ninja, err := exec.LookPath("ninja")
	if err != nil {
		t.Skip("ninja is not installed")
	}
	outDir := t.TempDir()
	inputPath := filepath.Join(outDir, "input")
	outputPath := filepath.Join(outDir, "output")
	runsPath := filepath.Join(outDir, "runs")
	buildFile := "rule generate\n" +
		"  command = cp $in $out && printf 'run\\n' >> runs\n" +
		"build output: generate input\n"
	if err := os.WriteFile(filepath.Join(outDir, "build.ninja"), []byte(buildFile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("complete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runNinja := func() {
		command := exec.Command(ninja, "-C", outDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("ninja failed: %v\n%s", err, output)
		}
	}
	runNinja()
	if err := checkpointNinjaState(outDir, false, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("interrupted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(outputPath, future, future); err != nil {
		t.Fatal(err)
	}
	if err := markNinjaRecoveryRequired(outDir); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, true); err != nil {
		t.Fatal(err)
	}
	runNinja()
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "interrupted\n" {
		t.Fatalf("trusted recovery rebuilt output: %q", output)
	}
	runs, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(runs), "run\n") != 1 {
		t.Fatalf("trusted recovery re-executed the action: %q", runs)
	}
}

func TestInitialRecoveryValidatesWithoutTrust(t *testing.T) {
	outDir := t.TempDir()
	outputPath := filepath.Join(outDir, "output")
	if err := os.WriteFile(outputPath, []byte("complete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf("1\t2\t%d\toutput\thash", info.ModTime().UnixNano())
	currentPath := filepath.Join(outDir, ".ninja_log")
	if err := os.WriteFile(currentPath, []byte(testNinjaLog(entry)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("interrupted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(outputPath, future, future); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}
	recovered, err := readNinjaLog(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.lines) != 0 {
		t.Fatalf("untrusted initial recovery retained invalid progress: %v", recovered.lines)
	}
}

func TestActualNinjaInterruptionRecoversIncrementally(t *testing.T) {
	ninja, err := exec.LookPath("ninja")
	if err != nil {
		t.Skip("ninja is not installed")
	}
	outDir := t.TempDir()
	buildFile := "rule generate\n" +
		"  command = ./generator.sh $in $out\n" +
		"build output: generate input\n"
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"if [ -e interrupt ]; then\n" +
		"  printf 'interrupted\\n' > \"$2\"\n" +
		"  : > marker\n" +
		"  sleep 300\n" +
		"fi\n" +
		"cp \"$1\" \"$2\"\n" +
		"printf 'run\\n' >> runs\n"
	if err := os.WriteFile(filepath.Join(outDir, "build.ninja"), []byte(buildFile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "generator.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "input"), []byte("complete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runNinja := func() {
		command := exec.Command(ninja, "-C", outDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("ninja failed: %v\n%s", err, output)
		}
	}
	runNinja()
	if err := checkpointNinjaState(outDir, false, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(outDir, "input"), []byte("complete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "interrupt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := markNinjaRecoveryRequired(outDir); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(ninja, "-C", outDir, "-d", "explain")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	cleanupRequired := true
	t.Cleanup(func() {
		if cleanupRequired {
			processes := signalProcessTree(pid, syscall.SIGKILL)
			waitForProcessTreeExit(pid, processes, time.Second)
		}
	})
	marker := filepath.Join(outDir, "marker")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			processes := signalProcessTree(pid, syscall.SIGKILL)
			_ = command.Wait()
			waitForProcessTreeExit(pid, processes, time.Second)
			t.Fatal("interrupted command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	processes := signalProcessTree(pid, syscall.SIGKILL)
	if err := command.Wait(); err == nil {
		t.Fatal("interrupted Ninja exited successfully")
	}
	if !waitForProcessTreeExit(pid, processes, time.Second) {
		t.Fatal("interrupted Ninja process group survived SIGKILL")
	}
	cleanupRequired = false
	if err := os.Remove(filepath.Join(outDir, "interrupt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, ".ninja_log"), []byte(ninjaLogHeader+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareNinjaState(outDir, false); err != nil {
		t.Fatal(err)
	}

	runNinja()
	if err := checkpointNinjaState(outDir, false, false); err != nil {
		t.Fatal(err)
	}
	if err := clearNinjaRecoveryRequired(outDir); err != nil {
		t.Fatal(err)
	}
	runNinja()
	output, err := os.ReadFile(filepath.Join(outDir, "output"))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "complete\n" {
		t.Fatalf("recovered output = %q", output)
	}
	runs, err := os.ReadFile(filepath.Join(outDir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(runs), "run\n") != 2 {
		t.Fatalf("incremental recovery executed the action %d times, want 2", strings.Count(string(runs), "run\n"))
	}
}
