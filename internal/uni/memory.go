// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MemorySnapshot struct {
	Total       int64
	Available   int64
	SwapTotal   int64
	SwapFree    int64
	SwapOutPage uint64
}

type SegmentSample struct {
	MinimumAvailable int64
	MinimumSwapFree  int64
	SwapOutBytes     uint64
	HighmemJobs      int
	R8Jobs           int
	JavaJobs         int
	KotlinJobs       int
}

func AnalysisMemoryLimit(total int64) int64 {
	if total <= 0 {
		return 0
	}
	reserve := max(4*gibibyte, total/5)
	limit := total - reserve
	return max(8*gibibyte, limit)
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
		if len(fields) == 2 && fields[0] == "pswpout" {
			snapshot.SwapOutPage, _ = strconv.ParseUint(fields[1], 10, 64)
			break
		}
	}
	return snapshot, scanner.Err()
}

func InitialJobs(maxJobs int, snapshot MemorySnapshot) int {
	limit := MaximumJobs(maxJobs)
	if ImminentOOM(snapshot.Available, snapshot.SwapFree) {
		return max(1, limit/2)
	}
	return limit
}

func MaximumJobs(maxJobs int) int {
	if maxJobs > 0 {
		return maxJobs
	}
	return runtime.NumCPU() + 2
}

func InitialBatchSize(configured, targets int, snapshot MemorySnapshot) int {
	batchSize := configured
	if ImminentOOM(snapshot.Available, snapshot.SwapFree) {
		batchSize = max(minimumBatchSize, int(math.Floor(float64(batchSize)*0.75)))
	} else {
		batchSize = maximumBatchSize
	}
	batchSize = min(maximumBatchSize, batchSize)
	if targets > 0 {
		batchSize = min(batchSize, targets)
	}
	return max(1, batchSize)
}

func ImminentOOM(available, swapFree int64) bool {
	return available < gibibyte || (available < 2*gibibyte && swapFree < 4*gibibyte)
}

func HighmemJobs(maxJobs int, snapshot MemorySnapshot) int {
	limit := MaximumJobs(maxJobs)
	if ImminentOOM(snapshot.Available, snapshot.SwapFree) {
		return max(1, limit/2)
	}
	return limit
}

func compilerPoolJobs(maxJobs int, snapshot MemorySnapshot, bytesPerJob int64) int {
	limit := MaximumJobs(maxJobs)
	budget := snapshot.Available - 4*gibibyte
	if budget <= 0 {
		return 1
	}
	return max(1, min(limit, int(budget/bytesPerJob)))
}

func JavaJobs(maxJobs int, snapshot MemorySnapshot) int {
	return compilerPoolJobs(maxJobs, snapshot, 2*gibibyte)
}

func KotlinJobs(maxJobs int, snapshot MemorySnapshot) int {
	return compilerPoolJobs(maxJobs, snapshot, 4*gibibyte)
}

func Adapt(batchSize, jobs, maxJobs int, sample SegmentSample) (int, int) {
	if ImminentOOM(sample.MinimumAvailable, sample.MinimumSwapFree) {
		batchSize = max(minimumBatchSize, int(math.Floor(float64(batchSize)*0.75)))
		jobs = max(1, int(math.Floor(float64(jobs)*0.75)))
	} else {
		batchSize = min(maximumBatchSize, batchSize*2)
		jobs = MaximumJobs(maxJobs)
	}
	if maxJobs > 0 {
		jobs = min(jobs, maxJobs)
	} else {
		jobs = min(jobs, runtime.NumCPU()+2)
	}
	return batchSize, max(1, jobs)
}

type memoryMonitor struct {
	stop        chan struct{}
	done        chan struct{}
	once        sync.Once
	mu          sync.Mutex
	min         int64
	minSwapFree int64
	base        uint64
	last        uint64
}

func startMemoryMonitor() *memoryMonitor {
	monitor := &memoryMonitor{
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		min:         math.MaxInt64,
		minSwapFree: math.MaxInt64,
	}
	if snapshot, err := ReadMemorySnapshot(); err == nil {
		monitor.min = snapshot.Available
		monitor.minSwapFree = snapshot.SwapFree
		monitor.base = snapshot.SwapOutPage
		monitor.last = snapshot.SwapOutPage
	}
	go func() {
		defer close(monitor.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				monitor.record()
			case <-monitor.stop:
				monitor.record()
				return
			}
		}
	}()
	return monitor
}

func (monitor *memoryMonitor) record() {
	snapshot, err := ReadMemorySnapshot()
	if err != nil {
		return
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	monitor.min = min(monitor.min, snapshot.Available)
	monitor.minSwapFree = min(monitor.minSwapFree, snapshot.SwapFree)
	monitor.last = snapshot.SwapOutPage
}

func (monitor *memoryMonitor) finish() SegmentSample {
	monitor.once.Do(func() { close(monitor.stop) })
	<-monitor.done
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	minimum := monitor.min
	if minimum == math.MaxInt64 {
		minimum = 0
	}
	minimumSwapFree := monitor.minSwapFree
	if minimumSwapFree == math.MaxInt64 {
		minimumSwapFree = 0
	}
	pages := uint64(0)
	if monitor.last > monitor.base {
		pages = monitor.last - monitor.base
	}
	return SegmentSample{
		MinimumAvailable: minimum,
		MinimumSwapFree:  minimumSwapFree,
		SwapOutBytes:     pages * uint64(os.Getpagesize()),
	}
}

func formatBytes(value int64) string {
	return fmt.Sprintf("%.1f GiB", float64(value)/gibibyte)
}
