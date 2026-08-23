// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOverrideEnvironment(t *testing.T) {
	base := []string{"A=old", "B=keep", "A=duplicate"}
	got := overrideEnvironment(base, "A=new", "C=value")
	want := []string{"B=keep", "A=new", "C=value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPositiveEnvironmentInt(t *testing.T) {
	environment := []string{"UNI_TEST_JOBS=12"}
	if value, ok := positiveEnvironmentInt(environment, "UNI_TEST_JOBS"); !ok || value != 12 {
		t.Fatalf("got %d, %t", value, ok)
	}
	environment = []string{"UNI_TEST_JOBS=0"}
	if _, ok := positiveEnvironmentInt(environment, "UNI_TEST_JOBS"); ok {
		t.Fatal("zero job count was accepted")
	}
	environment = []string{"UNI_TEST_JOBS=invalid"}
	if _, ok := positiveEnvironmentInt(environment, "UNI_TEST_JOBS"); ok {
		t.Fatal("invalid job count was accepted")
	}
}

func TestEnvironmentTrue(t *testing.T) {
	environment := []string{"A=false", "B=1", "A=yes"}
	if !environmentTrue(environment, "A") || !environmentTrue(environment, "B") {
		t.Fatal("true environment value was not recognized")
	}
	if environmentTrue(environment, "C") {
		t.Fatal("missing environment value was enabled")
	}
}

func TestPhasedNinjaExecutor(t *testing.T) {
	tests := map[string]string{
		"":        "ninja",
		"siso":    "ninja",
		"SISO":    "ninja",
		"ninja":   "ninja",
		"ninjago": "ninjago",
		"n2":      "n2",
	}
	for requested, expected := range tests {
		if actual := phasedNinjaExecutor(requested); actual != expected {
			t.Errorf("phasedNinjaExecutor(%q) = %q, want %q", requested, actual, expected)
		}
	}
}

func TestExecutorLabel(t *testing.T) {
	if actual := executorLabel(""); actual != "default" {
		t.Fatalf("empty executor label = %q", actual)
	}
	if actual := executorLabel("ninja"); actual != "ninja" {
		t.Fatalf("ninja executor label = %q", actual)
	}
}

func TestUniScopePrefix(t *testing.T) {
	first := uniScopePrefix("/tmp/out-a")
	if first != uniScopePrefix("/tmp/out-a/.") {
		t.Fatal("equivalent output paths produced different scope prefixes")
	}
	if first == uniScopePrefix("/tmp/out-b") {
		t.Fatal("different output paths produced the same scope prefix")
	}
}

func TestIsKotlinDaemonCommandLine(t *testing.T) {
	commandLine := []byte("java\x00-Xmx8g\x00org.jetbrains.kotlin.daemon.KotlinCompileDaemon\x00")
	if !isKotlinDaemonCommandLine(commandLine) {
		t.Fatal("Kotlin daemon command line was not recognized")
	}
	if isKotlinDaemonCommandLine([]byte("java\x00kotlin-incremental-client\x00")) {
		t.Fatal("Kotlin client was recognized as a reusable daemon")
	}
}

func TestTerminateProcessTree(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	cmd := exec.Command("sh", "-c", "setsid sh -c 'trap \"\" TERM; sleep 300' & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	cleanupRequired := true
	t.Cleanup(func() {
		if cleanupRequired {
			processes := signalProcessTree(pid, syscall.SIGKILL)
			waitForProcessTreeExit(pid, processes, time.Second)
		}
	})
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		terminateProcessTree(pid, "")
		close(done)
	}()
	if err := cmd.Wait(); err == nil {
		t.Fatal("process tree exited without a signal")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process tree termination timed out")
	}
	if !waitForProcessGroupExit(pid, time.Second) {
		t.Fatal("process group survived termination")
	}
	cleanupRequired = false
}

func TestSignalProcessTreeRejectsReusedPID(t *testing.T) {
	command := exec.Command("sleep", "300")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := readProcessIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-identity.PID, syscall.SIGKILL)
		_ = command.Wait()
	})
	reused := identity
	reused.StartTime++
	if processes := signalProcessTreeIdentity(reused, syscall.SIGKILL); len(processes) != 0 {
		t.Fatalf("mismatched process identity returned %d process(es)", len(processes))
	}
	if !sameProcess(identity) {
		t.Fatal("live process was killed through a reused PID identity")
	}
}

func TestTerminateBuildProcessTreeCleansTaggedOrphan(t *testing.T) {
	outDir := t.TempDir()
	statePath := filepath.Join(outDir, "uni", "product", "state.json")
	command := exec.Command("sh", "-c", "trap '' TERM; sleep 300")
	command.Env = append(os.Environ(), "UNI_STATE_FILE="+statePath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := readProcessIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRequired := true
	t.Cleanup(func() {
		if cleanupRequired {
			_ = syscall.Kill(-identity.PID, syscall.SIGKILL)
			_ = command.Wait()
		}
	})
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	terminateBuildProcessTree(processIdentity{PID: identity.PID, StartTime: identity.StartTime + 1}, "", outDir)
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("tagged orphan survived cleanup")
	}
	cleanupRequired = false
}

func TestRunWaitsForCancellationCleanup(t *testing.T) {
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "pid")
	script := filepath.Join(directory, "soong_ui.bash")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > \"$PID_FILE\"\ntrap '' TERM\nsleep 300 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PID_FILE", pidFile)
	runner := &commandRunner{
		top:         directory,
		baseEnv:     os.Environ(),
		outDir:      directory,
		soongUIPath: script,
		phasedNinja: "ninja",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.run(ctx, "--uni-ninja-mode", "only", filepath.Join(directory, "state.json"), nil, 1)
		done <- err
	}()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("test process did not start")
	}
	cleanupRequired := true
	t.Cleanup(func() {
		if cleanupRequired {
			processes := signalProcessTree(pid, syscall.SIGKILL)
			waitForProcessTreeExit(pid, processes, time.Second)
		}
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not finish cancellation cleanup")
	}
	if !waitForProcessGroupExit(pid, time.Second) {
		t.Fatal("runner returned before its process group exited")
	}
	cleanupRequired = false
	if _, err := os.Stat(ninjaRecoveryRequiredPath(directory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery marker remained after cancellation cleanup: %v", err)
	}
	if _, err := os.Stat(activeBuildLeasePath(directory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active build lease remained after cancellation cleanup: %v", err)
	}
}
