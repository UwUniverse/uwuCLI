// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	telemetryMonitorInterval = 30 * time.Second
	telemetryReportInterval  = time.Minute
)

type cpuCounters struct {
	total  uint64
	idle   uint64
	iowait uint64
}

type telemetryPoint struct {
	at         time.Time
	memory     MemorySnapshot
	memoryOK   bool
	psi        memoryPSI
	psiOK      bool
	cpu        cpuCounters
	cpuOK      bool
	cpuPSI     memoryPSI
	cpuPSIOK   bool
	ioPSI      memoryPSI
	ioPSIOK    bool
	load       loadSnapshot
	loadOK     bool
	diskFree   int64
	diskOK     bool
	cpuFreqMin int64
	cpuFreqAvg int64
	cpuFreqMax int64
	processes  []processTelemetry
}

type TelemetrySample struct {
	Timestamp       time.Time
	Elapsed         time.Duration
	RootPID         int
	MemoryAvailable int64
	SwapFree        int64
	SwapInBytes     uint64
	SwapOutBytes    uint64
	MajorFaults     uint64
	OOMKills        uint64
	CPUPercent      float64
	IOWaitPercent   float64
	Load            loadSnapshot
	MemoryPSI       memoryPSI
	CPUPSI          memoryPSI
	IOPSI           memoryPSI
	Processes       int
	TrackedRSS      int64
	R8              int
	Linker          int
	Javac           int
	Kotlinc         int
	Rustc           int
	Clang           int
	DiskAvailable   int64
	CPUFreqMinKHz   int64
	CPUFreqAvgKHz   int64
	CPUFreqMaxKHz   int64
}

type pressureSample struct {
	avg10  float64
	avg60  float64
	avg300 float64
	total  uint64
}

type memoryPSI struct {
	some pressureSample
	full pressureSample
}

type processTelemetry struct {
	identity processIdentity
	name     string
	taskType string
	rss      int64
}

type memoryMonitor struct {
	root         processIdentity
	outDir       string
	sink         func(TelemetrySample)
	started      time.Time
	lastReported time.Time

	stop chan struct{}
	done chan struct{}
	once sync.Once
	mu   sync.Mutex

	previous       telemetryPoint
	havePrevious   bool
	minimum        int64
	minimumSwap    int64
	baseSwapIn     uint64
	baseSwapOut    uint64
	lastSwapIn     uint64
	lastSwapOut    uint64
	initialMemory  MemorySnapshot
	finalMemory    MemorySnapshot
	haveMemory     bool
	maxPSISome     pressureSample
	maxPSIFull     pressureSample
	maxSwapInRate  float64
	maxSwapOutRate float64
	cpuTotal       float64
	cpuSamples     int
	maxCPU         float64
	samples        int
	maxProcesses   int
	maxTrackedRSS  int64
	maxCounts      map[string]int
	topRSS         map[[2]uint64]ProcessRSS
	warnings       []string
	warningSet     map[string]struct{}
}

func startMemoryMonitor(root processIdentity, outDir string, sink func(TelemetrySample)) *memoryMonitor {
	monitor := &memoryMonitor{
		root:        root,
		outDir:      outDir,
		sink:        sink,
		started:     time.Now(),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		minimum:     math.MaxInt64,
		minimumSwap: math.MaxInt64,
		maxCounts:   make(map[string]int),
		topRSS:      make(map[[2]uint64]ProcessRSS),
		warningSet:  make(map[string]struct{}),
	}
	monitor.record(true)
	go func() {
		defer close(monitor.done)
		ticker := time.NewTicker(telemetryMonitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				monitor.record(false)
			case <-monitor.stop:
				monitor.record(true)
				return
			}
		}
	}()
	return monitor
}

func (monitor *memoryMonitor) warn(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	if _, exists := monitor.warningSet[message]; exists {
		return
	}
	monitor.warningSet[message] = struct{}{}
	monitor.warnings = append(monitor.warnings, message)
}

func readMemoryPSI() (memoryPSI, error) {
	return readPSI("/proc/pressure/memory")
}

func readPSI(path string) (memoryPSI, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return memoryPSI{}, err
	}
	var result memoryPSI
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var sample pressureSample
		for _, field := range fields[1:] {
			name, value, found := strings.Cut(field, "=")
			if !found {
				continue
			}
			switch name {
			case "avg10":
				sample.avg10, _ = strconv.ParseFloat(value, 64)
			case "avg60":
				sample.avg60, _ = strconv.ParseFloat(value, 64)
			case "avg300":
				sample.avg300, _ = strconv.ParseFloat(value, 64)
			case "total":
				sample.total, _ = strconv.ParseUint(value, 10, 64)
			}
		}
		switch fields[0] {
		case "some":
			result.some = sample
		case "full":
			result.full = sample
		}
	}
	return result, nil
}

func readCPUCounters() (cpuCounters, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuCounters{}, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, fmt.Errorf("invalid /proc/stat cpu line")
	}
	values := make([]uint64, len(fields)-1)
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuCounters{}, err
		}
		values[index] = value
	}
	var total uint64
	// guest and guest_nice are already included in user and nice.
	for _, value := range values[:min(len(values), 8)] {
		total += value
	}
	iowait := uint64(0)
	if len(values) > 4 {
		iowait = values[4]
	}
	return cpuCounters{total: total, idle: values[3], iowait: iowait}, nil
}

func readDiskAvailable(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func readCPUFrequencySnapshot() (int64, int64, int64) {
	paths, _ := filepath.Glob("/sys/devices/system/cpu/cpufreq/policy*/scaling_cur_freq")
	var minimum, total, maximum int64
	var count int64
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		if minimum == 0 || value < minimum {
			minimum = value
		}
		maximum = max(maximum, value)
		total += value
		count++
	}
	if count == 0 {
		return 0, 0, 0
	}
	return minimum, total / count, maximum
}

func classifyBuildProcess(command []byte, fallback string) (string, string) {
	arguments := bytes.Split(bytes.TrimRight(command, "\x00"), []byte{0})
	name := fallback
	if len(arguments) > 0 && len(arguments[0]) > 0 {
		name = filepath.Base(string(arguments[0]))
	}
	joined := strings.ToLower(strings.Join(func() []string {
		values := make([]string, 0, len(arguments))
		for _, argument := range arguments {
			values = append(values, string(argument))
		}
		return values
	}(), " "))
	lowerName := strings.ToLower(name)
	isJava := lowerName == "java"
	switch {
	case isJava && (strings.Contains(joined, "com.android.tools.r8.r8") || strings.Contains(joined, " r8.jar")):
		return name, "r8"
	case strings.Contains(lowerName, "kotlinc") || isJava && (strings.Contains(joined, "kotlinc") || strings.Contains(joined, "kotlin-compiler")):
		return name, "kotlinc"
	case lowerName == "javac" || isJava && (strings.Contains(joined, " javac ") || strings.Contains(joined, "javac.jar")):
		return name, "javac"
	case lowerName == "rustc":
		return name, "rustc"
	case lowerName == "clang" || lowerName == "clang++":
		return name, "clang"
	case lowerName == "ld" || lowerName == "ld.lld" || lowerName == "lld":
		return name, "linker"
	default:
		return name, "other"
	}
}

func readProcessTelemetry(identity processIdentity) (processTelemetry, error) {
	base := filepath.Join("/proc", strconv.Itoa(identity.PID))
	command, commandErr := os.ReadFile(filepath.Join(base, "cmdline"))
	comm, _ := os.ReadFile(filepath.Join(base, "comm"))
	name, taskType := classifyBuildProcess(command, strings.TrimSpace(string(comm)))
	statm, err := os.ReadFile(filepath.Join(base, "statm"))
	if err != nil {
		return processTelemetry{}, err
	}
	fields := strings.Fields(string(statm))
	if len(fields) < 2 {
		return processTelemetry{}, fmt.Errorf("invalid statm for pid %d", identity.PID)
	}
	residentPages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return processTelemetry{}, err
	}
	if commandErr != nil && name == "" {
		return processTelemetry{}, commandErr
	}
	return processTelemetry{
		identity: identity,
		name:     name,
		taskType: taskType,
		rss:      residentPages * int64(os.Getpagesize()),
	}, nil
}

func readTelemetryPoint(root processIdentity, outDir string, detailed bool) (telemetryPoint, []error) {
	point := telemetryPoint{at: time.Now()}
	var errs []error
	var err error
	point.memory, err = ReadMemorySnapshot()
	if err != nil {
		errs = append(errs, fmt.Errorf("read memory telemetry: %w", err))
	} else {
		point.memoryOK = true
	}
	point.psi, err = readMemoryPSI()
	if err != nil {
		errs = append(errs, fmt.Errorf("read memory PSI: %w", err))
	} else {
		point.psiOK = true
	}
	point.cpu, err = readCPUCounters()
	if err != nil {
		errs = append(errs, fmt.Errorf("read CPU telemetry: %w", err))
	} else {
		point.cpuOK = true
	}
	if detailed {
		point.cpuPSI, err = readPSI("/proc/pressure/cpu")
		if err != nil {
			errs = append(errs, fmt.Errorf("read CPU PSI: %w", err))
		} else {
			point.cpuPSIOK = true
		}
		point.ioPSI, err = readPSI("/proc/pressure/io")
		if err != nil {
			errs = append(errs, fmt.Errorf("read IO PSI: %w", err))
		} else {
			point.ioPSIOK = true
		}
		point.load, err = readLoadSnapshot()
		if err != nil {
			errs = append(errs, fmt.Errorf("read load telemetry: %w", err))
		} else {
			point.loadOK = true
		}
		point.diskFree, err = readDiskAvailable(outDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read output disk telemetry: %w", err))
		} else {
			point.diskOK = true
		}
		point.cpuFreqMin, point.cpuFreqAvg, point.cpuFreqMax = readCPUFrequencySnapshot()
	}
	for _, identity := range snapshotProcessTreeForRoot(root) {
		process, err := readProcessTelemetry(identity)
		if err == nil {
			point.processes = append(point.processes, process)
		}
	}
	return point, errs
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func (monitor *memoryMonitor) record(forceReport bool) {
	monitor.mu.Lock()
	shouldReport := monitor.sink != nil && (forceReport || monitor.lastReported.IsZero() || time.Since(monitor.lastReported) >= telemetryReportInterval)
	monitor.mu.Unlock()
	point, errs := readTelemetryPoint(monitor.root, monitor.outDir, shouldReport)
	monitor.mu.Lock()
	for _, err := range errs {
		monitor.warn(err)
	}
	monitor.samples++
	if point.memoryOK {
		if !monitor.haveMemory {
			monitor.initialMemory = point.memory
			monitor.baseSwapIn = point.memory.SwapInPage
			monitor.baseSwapOut = point.memory.SwapOutPage
			monitor.haveMemory = true
		}
		monitor.finalMemory = point.memory
		monitor.minimum = min(monitor.minimum, point.memory.Available)
		monitor.minimumSwap = min(monitor.minimumSwap, point.memory.SwapFree)
		monitor.lastSwapIn = point.memory.SwapInPage
		monitor.lastSwapOut = point.memory.SwapOutPage
	}
	if point.psiOK {
		monitor.maxPSISome.avg10 = math.Max(monitor.maxPSISome.avg10, point.psi.some.avg10)
		monitor.maxPSISome.avg60 = math.Max(monitor.maxPSISome.avg60, point.psi.some.avg60)
		monitor.maxPSISome.avg300 = math.Max(monitor.maxPSISome.avg300, point.psi.some.avg300)
		monitor.maxPSISome.total = max(monitor.maxPSISome.total, point.psi.some.total)
		monitor.maxPSIFull.avg10 = math.Max(monitor.maxPSIFull.avg10, point.psi.full.avg10)
		monitor.maxPSIFull.avg60 = math.Max(monitor.maxPSIFull.avg60, point.psi.full.avg60)
		monitor.maxPSIFull.avg300 = math.Max(monitor.maxPSIFull.avg300, point.psi.full.avg300)
		monitor.maxPSIFull.total = max(monitor.maxPSIFull.total, point.psi.full.total)
	}
	cpuPercent := 0.0
	ioWaitPercent := 0.0
	if monitor.havePrevious {
		seconds := point.at.Sub(monitor.previous.at).Seconds()
		if seconds > 0 {
			if point.memoryOK && monitor.previous.memoryOK {
				pageSize := float64(os.Getpagesize())
				monitor.maxSwapInRate = math.Max(monitor.maxSwapInRate,
					float64(counterDelta(point.memory.SwapInPage, monitor.previous.memory.SwapInPage))*pageSize/seconds)
				monitor.maxSwapOutRate = math.Max(monitor.maxSwapOutRate,
					float64(counterDelta(point.memory.SwapOutPage, monitor.previous.memory.SwapOutPage))*pageSize/seconds)
			}
			if point.cpuOK && monitor.previous.cpuOK {
				totalDelta := counterDelta(point.cpu.total, monitor.previous.cpu.total)
				idleDelta := counterDelta(point.cpu.idle, monitor.previous.cpu.idle)
				ioWaitDelta := counterDelta(point.cpu.iowait, monitor.previous.cpu.iowait)
				if totalDelta > 0 && idleDelta+ioWaitDelta <= totalDelta {
					cpuPercent = 100 * float64(totalDelta-idleDelta-ioWaitDelta) / float64(totalDelta)
					ioWaitPercent = 100 * float64(ioWaitDelta) / float64(totalDelta)
					monitor.cpuTotal += cpuPercent
					monitor.cpuSamples++
					monitor.maxCPU = math.Max(monitor.maxCPU, cpuPercent)
				}
			}
		}
	}
	counts := make(map[string]int)
	var trackedRSS int64
	for _, process := range point.processes {
		counts[process.taskType]++
		trackedRSS += process.rss
		key := [2]uint64{uint64(process.identity.PID), process.identity.StartTime}
		if existing, ok := monitor.topRSS[key]; !ok || process.rss > existing.RSSBytes {
			monitor.topRSS[key] = ProcessRSS{
				PID: process.identity.PID, Name: process.name, TaskType: process.taskType,
				RSSBytes: process.rss, Timestamp: point.at,
			}
		}
	}
	monitor.maxProcesses = max(monitor.maxProcesses, len(point.processes))
	monitor.maxTrackedRSS = max(monitor.maxTrackedRSS, trackedRSS)
	for taskType, count := range counts {
		monitor.maxCounts[taskType] = max(monitor.maxCounts[taskType], count)
	}
	var reportSample TelemetrySample
	if shouldReport {
		reportSample = TelemetrySample{
			Timestamp: point.at, Elapsed: point.at.Sub(monitor.started), RootPID: monitor.root.PID,
			CPUPercent: cpuPercent, IOWaitPercent: ioWaitPercent, Load: point.load,
			MemoryPSI: point.psi, CPUPSI: point.cpuPSI, IOPSI: point.ioPSI,
			Processes: len(point.processes), TrackedRSS: trackedRSS,
			R8: counts["r8"], Linker: counts["linker"], Javac: counts["javac"],
			Kotlinc: counts["kotlinc"], Rustc: counts["rustc"], Clang: counts["clang"],
			CPUFreqMinKHz: point.cpuFreqMin, CPUFreqAvgKHz: point.cpuFreqAvg, CPUFreqMaxKHz: point.cpuFreqMax,
		}
		if point.memoryOK {
			reportSample.MemoryAvailable = point.memory.Available
			reportSample.SwapFree = point.memory.SwapFree
			reportSample.SwapInBytes = counterDelta(point.memory.SwapInPage, monitor.baseSwapIn) * uint64(os.Getpagesize())
			reportSample.SwapOutBytes = counterDelta(point.memory.SwapOutPage, monitor.baseSwapOut) * uint64(os.Getpagesize())
			reportSample.MajorFaults = counterDelta(point.memory.MajorFaults, monitor.initialMemory.MajorFaults)
			reportSample.OOMKills = counterDelta(point.memory.OOMKills, monitor.initialMemory.OOMKills)
		}
		if point.diskOK {
			reportSample.DiskAvailable = point.diskFree
		}
		monitor.lastReported = point.at
	}
	monitor.previous = point
	monitor.havePrevious = true
	monitor.mu.Unlock()
	if shouldReport && monitor.sink != nil {
		monitor.sink(reportSample)
	}
}

func (monitor *memoryMonitor) finish() SegmentSample {
	monitor.once.Do(func() { close(monitor.stop) })
	<-monitor.done
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	minimum := monitor.minimum
	if minimum == math.MaxInt64 {
		minimum = 0
	}
	minimumSwap := monitor.minimumSwap
	if minimumSwap == math.MaxInt64 {
		minimumSwap = 0
	}
	top := make([]ProcessRSS, 0, len(monitor.topRSS))
	for _, process := range monitor.topRSS {
		top = append(top, process)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].RSSBytes > top[j].RSSBytes })
	if len(top) > 5 {
		top = top[:5]
	}
	averageCPU := 0.0
	if monitor.cpuSamples > 0 {
		averageCPU = monitor.cpuTotal / float64(monitor.cpuSamples)
	}
	return SegmentSample{
		InitialAvailable: monitor.initialMemory.Available,
		FinalAvailable:   monitor.finalMemory.Available,
		MinimumAvailable: minimum,
		InitialSwapFree:  monitor.initialMemory.SwapFree,
		FinalSwapFree:    monitor.finalMemory.SwapFree,
		MinimumSwapFree:  minimumSwap,
		MaxPSISomeAvg10:  monitor.maxPSISome.avg10,
		MaxPSISomeAvg60:  monitor.maxPSISome.avg60,
		MaxPSISomeAvg300: monitor.maxPSISome.avg300,
		MaxPSIFullAvg10:  monitor.maxPSIFull.avg10,
		MaxPSIFullAvg60:  monitor.maxPSIFull.avg60,
		MaxPSIFullAvg300: monitor.maxPSIFull.avg300,
		MaxSwapInRate:    monitor.maxSwapInRate,
		MaxSwapOutRate:   monitor.maxSwapOutRate,
		AverageCPU:       averageCPU,
		MaxCPU:           monitor.maxCPU,
		Samples:          monitor.samples,
		MaxProcesses:     monitor.maxProcesses,
		MaxTrackedRSS:    monitor.maxTrackedRSS,
		MaxR8:            monitor.maxCounts["r8"],
		MaxLinker:        monitor.maxCounts["linker"],
		MaxJavac:         monitor.maxCounts["javac"],
		MaxKotlinc:       monitor.maxCounts["kotlinc"],
		MaxRustc:         monitor.maxCounts["rustc"],
		MaxClang:         monitor.maxCounts["clang"],
		TopRSS:           top,
		Warnings:         append([]string(nil), monitor.warnings...),
		SwapInBytes:      counterDelta(monitor.lastSwapIn, monitor.baseSwapIn) * uint64(os.Getpagesize()),
		SwapOutBytes:     counterDelta(monitor.lastSwapOut, monitor.baseSwapOut) * uint64(os.Getpagesize()),
		MajorFaults:      counterDelta(monitor.finalMemory.MajorFaults, monitor.initialMemory.MajorFaults),
		OOMKills:         counterDelta(monitor.finalMemory.OOMKills, monitor.initialMemory.OOMKills),
	}
}
