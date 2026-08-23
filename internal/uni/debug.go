// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type debugReport struct {
	file    *os.File
	path    string
	started time.Time
}

func newDebugReport(outDir, product string, args []string) (*debugReport, error) {
	started := time.Now()
	path := filepath.Join(outDir, "uwuCli-debug-report_"+started.Format("20060102-150405.000000000")+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}
	report := &debugReport{file: file, path: path, started: started}
	report.write("started=%s\nproduct=%s\narguments=%s\ncpus=%d\n",
		started.Format(time.RFC3339Nano), product, strings.Join(args, " "), runtime.NumCPU())
	report.system("start", outDir)
	return report, nil
}

func (report *debugReport) write(format string, args ...any) {
	if report == nil || report.file == nil {
		return
	}
	_, _ = fmt.Fprintf(report.file, format, args...)
}

func (report *debugReport) system(label, outDir string) {
	if report == nil {
		return
	}
	snapshot, err := ReadMemorySnapshot()
	if err != nil {
		report.write("system label=%s error=%q\n", label, err)
		return
	}
	load := "unknown"
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 3 {
			load = strings.Join(fields[:3], ",")
		}
	}
	diskFree := uint64(0)
	var stat syscall.Statfs_t
	if syscall.Statfs(outDir, &stat) == nil {
		diskFree = stat.Bavail * uint64(stat.Bsize)
	}
	report.write("system label=%s mem_available=%s swap_free=%s swap_used=%s load=%s out_free=%s\n",
		label, formatBytes(snapshot.Available), formatBytes(snapshot.SwapFree),
		formatBytes(snapshot.SwapTotal-snapshot.SwapFree), load, formatBytes(int64(diskFree)))
}

func (report *debugReport) phase(name string, started time.Time, sample SegmentSample, err error, outDir string) {
	if report == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	report.write("phase name=%s elapsed=%s highmem_jobs=%d r8_jobs=%d java_jobs=%d kotlin_jobs=%d min_mem_available=%s min_swap_free=%s swap_out=%s result=%q\n",
		name, time.Since(started).Round(time.Millisecond), sample.HighmemJobs, sample.R8Jobs, sample.JavaJobs, sample.KotlinJobs, formatBytes(sample.MinimumAvailable),
		formatBytes(sample.MinimumSwapFree), formatBytes(int64(sample.SwapOutBytes)), result)
	report.system(name+"-end", outDir)
}

func (report *debugReport) analysis(name, source string, modules int, started time.Time, err error, outDir string) {
	if report == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	report.write("analysis name=%s source=%s modules=%d elapsed=%s result=%q\n",
		name, source, modules, time.Since(started).Round(time.Millisecond), result)
	report.system(name+"-end", outDir)
}

func (report *debugReport) event(format string, args ...any) {
	if report == nil {
		return
	}
	report.write("event "+format+"\n", args...)
}

func (report *debugReport) close(outDir string) {
	if report == nil || report.file == nil {
		return
	}
	report.system("finish", outDir)
	report.write("elapsed=%s\n", time.Since(report.started).Round(time.Millisecond))
	_ = report.file.Close()
	report.file = nil
}
