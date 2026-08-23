// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestActiveBuildLeaseBlocksConcurrentRun(t *testing.T) {
	outDir := t.TempDir()
	lease, err := writeActiveBuildLease(outDir, os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	active, err := activeBuildStillRunning(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if active == "" {
		t.Fatal("live build lease was ignored")
	}
	if err := clearActiveBuildLease(outDir, lease.Token); err != nil {
		t.Fatal(err)
	}
	active, err = activeBuildStillRunning(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Fatalf("cleared build lease is still active: %s", active)
	}
}

func TestMalformedActiveBuildLeaseIsRemoved(t *testing.T) {
	outDir := t.TempDir()
	path := activeBuildLeasePath(outDir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	active, err := activeBuildStillRunning(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Fatalf("malformed build lease is active: %s", active)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("malformed build lease was not removed: %v", err)
	}
}

func TestOrphanedUniProcessIsDetected(t *testing.T) {
	outDir := t.TempDir()
	statePath := filepath.Join(outDir, "uni", "product", "state.json")
	command := exec.Command("sh", "-c", "sleep 300")
	command.Env = append(os.Environ(), "UNI_STATE_FILE="+statePath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	cleanupRequired := true
	t.Cleanup(func() {
		if cleanupRequired {
			terminateProcessTree(pid, "")
			_ = command.Wait()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		active, err := activeBuildStillRunning(outDir)
		if err != nil {
			t.Fatal(err)
		}
		if active != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphaned uni process was not detected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	terminated := make(chan struct{})
	go func() {
		terminateProcessTree(pid, "")
		close(terminated)
	}()
	_ = command.Wait()
	<-terminated
	cleanupRequired = false
}
