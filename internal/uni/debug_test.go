// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebugReport(t *testing.T) {
	directory := t.TempDir()
	report, err := newDebugReport(directory, "uwu_nabu", []string{"-j18", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	report.analysis("r8", "soong", 77, time.Now(), nil, directory)
	report.phase("final", time.Now(), SegmentSample{JavaJobs: 11, KotlinJobs: 6}, nil, directory)
	path := report.path
	report.close(directory)
	if !strings.HasPrefix(filepath.Base(path), "uwuCli-debug-report_") {
		t.Fatalf("unexpected report name: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "analysis name=r8 source=soong modules=77") ||
		!strings.Contains(text, "java_jobs=11 kotlin_jobs=6") ||
		!strings.Contains(text, "system label=start") {
		t.Fatalf("incomplete debug report:\n%s", text)
	}
}
