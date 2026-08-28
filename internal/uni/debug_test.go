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
	report.telemetry("final", TelemetrySample{
		Timestamp: time.Now(), RootPID: os.Getpid(), CPUPercent: 91.5,
		MemoryAvailable: 8 * gibibyte, OOMKills: 1, DiskAvailable: 30 * gibibyte,
		Processes: 18, Clang: 12,
	})
	report.phase("final", time.Now(), SegmentSample{
		Samples: 3, RustJobs: 5, JavaJobs: 11, KotlinJobs: 6,
		AnalysisLimit: 24 * gibibyte, AnalysisGC: 500,
		InitialAvailable: 12 * gibibyte, FinalAvailable: 10 * gibibyte,
		MinimumAvailable: 9 * gibibyte, MaxProcesses: 42, MaxTrackedRSS: 5 * gibibyte,
	}, nil, directory)
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
	if !strings.Contains(text, "report_version=3") ||
		!strings.Contains(text, `arguments="-j18" "otapackage"`) ||
		!strings.Contains(text, "host hostname=") ||
		!strings.Contains(text, "analysis name=r8 source=soong modules=77") ||
		!strings.Contains(text, "rust_jobs=5 java_jobs=11 kotlin_jobs=6") ||
		!strings.Contains(text, "analysis_memory_limit_bytes=25769803776") ||
		!strings.Contains(text, "analysis_gc_percent=500") ||
		!strings.Contains(text, "telemetry_samples=3") ||
		!strings.Contains(text, "max_processes=42") ||
		!strings.Contains(text, "telemetry phase=final") ||
		!strings.Contains(text, "cpu=91.5") ||
		!strings.Contains(text, "oom_kills=1") ||
		!strings.Contains(text, "system label=start") ||
		!strings.Contains(text, "mem_total_bytes=") ||
		!strings.Contains(text, "psi_some_avg60=") ||
		!strings.Contains(text, "out_available_bytes=") {
		t.Fatalf("incomplete debug report:\n%s", text)
	}
}

func TestDebugReportRotation(t *testing.T) {
	directory := t.TempDir()
	for index := 0; index < automaticDebugReportLimit+3; index++ {
		report, err := newDebugReport(directory, "uwu_nabu", nil)
		if err != nil {
			t.Fatal(err)
		}
		report.close(directory)
	}
	matches, err := filepath.Glob(filepath.Join(directory, "uwuCli-debug-report_*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != automaticDebugReportLimit {
		t.Fatalf("kept %d debug reports, want %d", len(matches), automaticDebugReportLimit)
	}
}

func TestCleanBuildLogsPreservesBuildState(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"verbose.log.gz", "uwuCli-debug-report_1.log", "uwuCli-output_1.log", ".ninja_log", "product.img"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0666); err != nil {
			t.Fatal(err)
		}
	}
	cleaned, err := cleanBuildLogs(directory)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.files != 3 {
		t.Fatalf("removed %d files, want 3", cleaned.files)
	}
	for _, name := range []string{".ninja_log", "product.img"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatalf("build state %s was removed: %v", name, err)
		}
	}
}
