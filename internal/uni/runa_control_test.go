/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package uni

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveExecutorPathPrefersLinuxAMD64PrebuiltRuna(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("vendor Runa prebuilt is Linux x86_64 only")
	}
	t.Setenv("OUT_DIR", "")
	top := t.TempDir()
	prebuilt := filepath.Join(top, "vendor", "uwu-prebuilts", "runa", "runa")
	built := filepath.Join(top, "out", "host", "linux-x86", "bin", "runa")
	for _, path := range []string{prebuilt, built} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stub"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := resolveExecutorPath("runa", top)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(prebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("resolved Runa = %q, want prebuilt %q", resolved, want)
	}
}

func TestResolveExecutorPathFallsBackToBuiltRuna(t *testing.T) {
	t.Setenv("OUT_DIR", "")
	top := t.TempDir()
	built := filepath.Join(top, "out", "host", "linux-x86", "bin", "runa")
	if err := os.MkdirAll(filepath.Dir(built), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveExecutorPath("runa", top)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(built)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("resolved Runa = %q, want Soong-built binary %q", resolved, want)
	}
}
