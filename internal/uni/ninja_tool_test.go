// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNinjaToolSourceNewer(t *testing.T) {
	directory := t.TempDir()
	sourceDir := filepath.Join(directory, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "src", "graph.cc")
	binaryPath := filepath.Join(directory, "ninja")
	if err := os.WriteFile(sourcePath, []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(sourcePath, now.Add(-time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(binaryPath, now, now); err != nil {
		t.Fatal(err)
	}
	newer, err := ninjaToolSourceNewer(sourceDir, binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if newer {
		t.Fatal("older source forced a Ninja rebuild")
	}
	if err := os.Chtimes(sourcePath, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	newer, err = ninjaToolSourceNewer(sourceDir, binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !newer {
		t.Fatal("newer source did not force a Ninja rebuild")
	}
}

func TestEnsureAssumeExistingNinjaIntegration(t *testing.T) {
	top := os.Getenv("UNI_TEST_TOP")
	if top == "" {
		t.Skip("UNI_TEST_TOP is not set")
	}
	outDir := filepath.Join(t.TempDir(), "out")
	path, err := ensureAssumeExistingNinja(top, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAssumeExistingNinja(path); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	buildFile := "rule generate\n" +
		"  command = cp input output && printf 'run\\n' >> marker\n" +
		"build output: generate input\n"
	if err := os.WriteFile(filepath.Join(workspace, "build.ninja"), []byte(buildFile), 0644); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(workspace, "input")
	outputPath := filepath.Join(workspace, "output")
	markerPath := filepath.Join(workspace, "marker")
	if err := os.WriteFile(inputPath, []byte("input"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	runNinja := func(args ...string) {
		command := exec.Command(path, args...)
		command.Dir = workspace
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Ninja failed: %v\n%s", err, output)
		}
	}
	runNinja("output")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("normal Ninja trusted an unlogged output: %v", err)
	}
	for _, candidate := range []string{filepath.Join(workspace, ".ninja_log"), markerPath} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(outputPath, future, future); err != nil {
		t.Fatal(err)
	}
	runNinja("-d", "assumeexisting", "output")
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("assumeexisting rebuilt an existing output: %v", err)
	}
	newerInput := future.Add(2 * time.Second)
	if err := os.Chtimes(inputPath, newerInput, newerInput); err != nil {
		t.Fatal(err)
	}
	runNinja("-d", "assumeexisting", "output")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("assumeexisting ignored a newer input: %v", err)
	}
}
