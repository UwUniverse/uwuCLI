// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type MemorySnapshot struct {
	Total       int64
	Available   int64
	SwapTotal   int64
	SwapFree    int64
	SwapInPage  uint64
	SwapOutPage uint64
	MajorFaults uint64
	OOMKills    uint64
}

type ProcessRSS struct {
	PID       int
	Name      string
	TaskType  string
	RSSBytes  int64
	Timestamp time.Time
}

type SegmentSample struct {
	InitialAvailable int64
	FinalAvailable   int64
	MinimumAvailable int64
	InitialSwapFree  int64
	FinalSwapFree    int64
	MinimumSwapFree  int64
	MaxPSISomeAvg10  float64
	MaxPSISomeAvg60  float64
	MaxPSISomeAvg300 float64
	MaxPSIFullAvg10  float64
	MaxPSIFullAvg60  float64
	MaxPSIFullAvg300 float64
	MaxSwapInRate    float64
	MaxSwapOutRate   float64
	AverageCPU       float64
	MaxCPU           float64
	Samples          int
	MaxProcesses     int
	MaxTrackedRSS    int64
	MaxR8            int
	MaxLinker        int
	MaxJavac         int
	MaxKotlinc       int
	MaxRustc         int
	MaxClang         int
	TopRSS           []ProcessRSS
	Warnings         []string
	SwapOutBytes     uint64
	SwapInBytes      uint64
	MajorFaults      uint64
	OOMKills         uint64
	HighmemJobs      int
	R8Jobs           int
	RustJobs         int
	JavaJobs         int
	KotlinJobs       int
	HighmemExplicit  bool
	R8Explicit       bool
	RustExplicit     bool
	JavaExplicit     bool
	KotlinExplicit   bool
	PoolReason       string
	AnalysisLimit    int64
	AnalysisGC       int
	Phase            string
	Duration         time.Duration
}

func AnalysisMemoryLimit(total, available int64) int64 {
	if total <= 0 {
		return 0
	}
	// Android.bp analysis has non-Go memory and is followed by Kati. Leaving a
	// physical reserve is faster than letting the Go heap reclaim through swap.
	reserve := max(4*gibibyte, total/5)
	limit := total - reserve
	if available > 2*gibibyte {
		limit = min(limit, available-2*gibibyte)
	}
	if limit > 0 {
		return limit
	}
	return max(1, total/2)
}

func ReadMemorySnapshot() (MemorySnapshot, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemorySnapshot{}, err
	}
	var snapshot MemorySnapshot
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		value *= 1024
		switch fields[0] {
		case "MemTotal:":
			snapshot.Total = value
		case "MemAvailable:":
			snapshot.Available = value
		case "SwapTotal:":
			snapshot.SwapTotal = value
		case "SwapFree:":
			snapshot.SwapFree = value
		}
	}
	file, err := os.Open("/proc/vmstat")
	if err != nil {
		return MemorySnapshot{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "pswpin":
			snapshot.SwapInPage, _ = strconv.ParseUint(fields[1], 10, 64)
		case "pswpout":
			snapshot.SwapOutPage, _ = strconv.ParseUint(fields[1], 10, 64)
		case "pgmajfault":
			snapshot.MajorFaults, _ = strconv.ParseUint(fields[1], 10, 64)
		case "oom_kill":
			snapshot.OOMKills, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return snapshot, scanner.Err()
}

func InitialJobs(maxJobs int, snapshot MemorySnapshot) int {
	return MaximumJobs(maxJobs)
}

func MaximumJobs(maxJobs int) int {
	if maxJobs > 0 {
		return maxJobs
	}
	return runtime.NumCPU() + 2
}

func InitialBatchSize(configured, targets int, snapshot MemorySnapshot) int {
	batchSize := min(configured, automaticMaximumBatchSize)
	if targets > 0 {
		batchSize = min(batchSize, targets)
	}
	return max(1, batchSize)
}

func HighmemJobs(maxJobs int, snapshot MemorySnapshot) int {
	return burstPoolJobs(maxJobs, snapshot)
}

func R8Jobs(maxJobs int, snapshot MemorySnapshot) int {
	return burstPoolJobs(maxJobs, snapshot)
}

func burstPoolJobs(maxJobs int, snapshot MemorySnapshot) int {
	limit := MaximumJobs(maxJobs)
	if snapshot.Available <= 0 || snapshot.Available >= 8*gibibyte {
		return limit
	}
	return max(1, min(limit, int(snapshot.Available/(2*gibibyte))))
}

func memoryPoolJobs(maxJobs int, snapshot MemorySnapshot, bytesPerJob int64) int {
	limit := MaximumJobs(maxJobs)
	if snapshot.Available <= 0 || bytesPerJob <= 0 {
		return limit
	}
	budget := snapshot.Available - 3*gibibyte
	if budget <= 0 {
		return 1
	}
	return max(1, min(limit, int(budget/bytesPerJob)))
}

func JavaJobs(maxJobs int, snapshot MemorySnapshot) int {
	return memoryPoolJobs(maxJobs, snapshot, 2*gibibyte)
}

func RustJobs(maxJobs int, snapshot MemorySnapshot, codegenUnits int) int {
	limit := MaximumJobs(maxJobs)
	if codegenUnits < 1 {
		codegenUnits = 1
	}
	// Ninja accounts for a rustc process as one job while LLVM can run one
	// backend worker per codegen unit. Bound both layers so Rust still fills the
	// machine without allowing every Ninja slot to fan out independently.
	cpuBound := max(1, (limit+codegenUnits-1)/codegenUnits)
	memoryBound := memoryPoolJobs(limit, snapshot, 2*gibibyte)
	return min(cpuBound, memoryBound)
}

func KotlinJobs(maxJobs int, snapshot MemorySnapshot) int {
	// Equal JVM arguments let the build-tools client reuse one Kotlin daemon.
	// Four GiB per admitted edge keeps six requests available on this host while
	// retaining physical memory for javac, clang and the daemon's 8 GiB heap.
	return memoryPoolJobs(maxJobs, snapshot, 4*gibibyte)
}

func formatBytes(value int64) string {
	return fmt.Sprintf("%.1f GiB", float64(value)/gibibyte)
}
