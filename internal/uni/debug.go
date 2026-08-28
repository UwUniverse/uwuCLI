// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func sourceRevision(path string) string {
	output, err := exec.Command("git", "-C", path, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

type debugReport struct {
	file          *os.File
	path          string
	started       time.Time
	ccacheStarted map[string]uint64
	mu            sync.Mutex
}

type loadSnapshot struct {
	one       string
	five      string
	fifteen   string
	runnable  string
	processes string
	lastPID   string
}

func quoteArguments(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

func readLoadSnapshot() (loadSnapshot, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return loadSnapshot{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 5 {
		return loadSnapshot{}, fmt.Errorf("invalid /proc/loadavg")
	}
	runnable, processes, _ := strings.Cut(fields[3], "/")
	return loadSnapshot{
		one: fields[0], five: fields[1], fifteen: fields[2],
		runnable: runnable, processes: processes, lastPID: fields[4],
	}, nil
}

func newDebugReport(outDir, product string, args []string) (*debugReport, error) {
	started := time.Now()
	path := filepath.Join(outDir, "uwuCli-debug-report_"+started.Format("20060102-150405.000000000")+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}
	report := &debugReport{file: file, path: path, started: started}
	if cleaned, cleanupErr := pruneDebugReports(outDir, automaticDebugReportLimit); cleanupErr != nil {
		report.write("debug_warning source=log_rotation error=%q\n", cleanupErr)
	} else if cleaned.files > 0 {
		report.write("log_rotation files=%d bytes=%d\n", cleaned.files, cleaned.bytes)
	}
	hostname, hostnameErr := os.Hostname()
	workingDirectory, workingDirectoryErr := os.Getwd()
	kernelData, kernelErr := os.ReadFile("/proc/sys/kernel/osrelease")
	report.write("report_version=3\nstarted=%s\nproduct=%q\narguments=%s\n",
		started.Format(time.RFC3339Nano), product, quoteArguments(args))
	report.write("process pid=%d ppid=%d uid=%d euid=%d\n", os.Getpid(), os.Getppid(), os.Getuid(), os.Geteuid())
	report.write("host hostname=%q kernel=%q os=%s arch=%s go=%s cpus=%d gomaxprocs=%d cwd=%q\n",
		hostname, strings.TrimSpace(string(kernelData)), runtime.GOOS, runtime.GOARCH, runtime.Version(),
		runtime.NumCPU(), runtime.GOMAXPROCS(0), workingDirectory)
	if hostnameErr != nil {
		report.write("debug_warning source=hostname error=%q\n", hostnameErr)
	}
	if workingDirectoryErr != nil {
		report.write("debug_warning source=cwd error=%q\n", workingDirectoryErr)
	}
	if kernelErr != nil {
		report.write("debug_warning source=kernel error=%q\n", kernelErr)
	}
	report.system("start", outDir)
	return report, nil
}

func (report *debugReport) write(format string, args ...any) {
	if report == nil {
		return
	}
	report.mu.Lock()
	defer report.mu.Unlock()
	if report.file == nil {
		return
	}
	_, _ = fmt.Fprintf(report.file, format, args...)
}

func (report *debugReport) telemetry(phase string, sample TelemetrySample) {
	if report == nil {
		return
	}
	report.write("telemetry phase=%s at=%s elapsed=%s root_pid=%d cpu=%.1f iowait=%.1f cpu_freq_avg_khz=%d load_1=%s processes=%d tracked_rss_bytes=%d mem_available_bytes=%d swap_free_bytes=%d swap_out_bytes=%d oom_kills=%d mem_psi_full_avg10=%.2f cpu_psi_some_avg10=%.2f io_psi_full_avg10=%.2f out_available_bytes=%d r8=%d linker=%d javac=%d kotlinc=%d rustc=%d clang=%d\n",
		phase, sample.Timestamp.Format(time.RFC3339Nano), sample.Elapsed.Round(time.Millisecond), sample.RootPID,
		sample.CPUPercent, sample.IOWaitPercent, sample.CPUFreqAvgKHz, sample.Load.one,
		sample.Processes, sample.TrackedRSS, sample.MemoryAvailable, sample.SwapFree,
		sample.SwapOutBytes, sample.OOMKills, sample.MemoryPSI.full.avg10, sample.CPUPSI.some.avg10,
		sample.IOPSI.full.avg10, sample.DiskAvailable,
		sample.R8, sample.Linker, sample.Javac, sample.Kotlinc, sample.Rustc, sample.Clang)
}

func readCcacheStats(environment []string) (map[string]uint64, error) {
	path, err := exec.LookPath("ccache")
	if err != nil {
		return nil, err
	}
	command := exec.Command(path, "--print-stats")
	command.Env = environment
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	stats := make(map[string]uint64)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			stats[fields[0]] = value
		}
	}
	return stats, nil
}

func (report *debugReport) ccache(label string, environment []string) {
	if report == nil {
		return
	}
	stats, err := readCcacheStats(environment)
	if err != nil {
		report.write("ccache_warning label=%s error=%q\n", label, err)
		return
	}
	report.mu.Lock()
	started := report.ccacheStarted
	if label == "start" {
		report.ccacheStarted = stats
	}
	report.mu.Unlock()
	delta := func(name string) uint64 {
		if started == nil {
			return 0
		}
		return counterDelta(stats[name], started[name])
	}
	report.write("ccache label=%s direct_hit=%d direct_miss=%d local_hit=%d local_miss=%d cache_size_kib=%d max_cache_size_kib=%d files=%d cleanups=%d compile_failed=%d internal_error=%d delta_direct_hit=%d delta_direct_miss=%d delta_local_hit=%d delta_local_miss=%d delta_cleanups=%d\n",
		label, stats["direct_cache_hit"], stats["direct_cache_miss"],
		stats["local_storage_hit"], stats["local_storage_miss"],
		stats["cache_size_kibibyte"], stats["max_cache_size_kibibyte"], stats["files_in_cache"],
		stats["cleanups_performed"], stats["compile_failed"], stats["internal_error"],
		delta("direct_cache_hit"), delta("direct_cache_miss"), delta("local_storage_hit"),
		delta("local_storage_miss"), delta("cleanups_performed"))
}

func (report *debugReport) system(label, outDir string) {
	if report == nil {
		return
	}
	snapshot, memoryErr := ReadMemorySnapshot()
	psi, psiErr := readMemoryPSI()
	load, loadErr := readLoadSnapshot()
	var stat syscall.Statfs_t
	diskErr := syscall.Statfs(outDir, &stat)
	if memoryErr != nil {
		report.write("system_warning label=%s source=memory error=%q\n", label, memoryErr)
	}
	if psiErr != nil {
		report.write("system_warning label=%s source=memory_psi error=%q\n", label, psiErr)
	}
	if loadErr != nil {
		report.write("system_warning label=%s source=load error=%q\n", label, loadErr)
	}
	if diskErr != nil {
		report.write("system_warning label=%s source=out_disk error=%q\n", label, diskErr)
	}
	blockSize := uint64(stat.Bsize)
	diskTotal := stat.Blocks * blockSize
	diskFree := stat.Bfree * blockSize
	diskAvailable := stat.Bavail * blockSize
	report.write("system label=%s at=%s mem_total_bytes=%d mem_available_bytes=%d mem_available=%q swap_total_bytes=%d swap_free_bytes=%d swap_used_bytes=%d swap_in_pages=%d swap_out_pages=%d major_faults=%d oom_kills=%d load_1=%s load_5=%s load_15=%s runnable=%s processes=%s last_pid=%s psi_some_avg10=%.2f psi_some_avg60=%.2f psi_some_avg300=%.2f psi_some_total_us=%d psi_full_avg10=%.2f psi_full_avg60=%.2f psi_full_avg300=%.2f psi_full_total_us=%d out_total_bytes=%d out_free_bytes=%d out_available_bytes=%d out_files=%d out_files_free=%d\n",
		label, time.Now().Format(time.RFC3339Nano), snapshot.Total, snapshot.Available,
		formatBytes(snapshot.Available), snapshot.SwapTotal, snapshot.SwapFree,
		snapshot.SwapTotal-snapshot.SwapFree, snapshot.SwapInPage, snapshot.SwapOutPage, snapshot.MajorFaults, snapshot.OOMKills,
		load.one, load.five, load.fifteen, load.runnable, load.processes, load.lastPID,
		psi.some.avg10, psi.some.avg60, psi.some.avg300, psi.some.total,
		psi.full.avg10, psi.full.avg60, psi.full.avg300, psi.full.total,
		diskTotal, diskFree, diskAvailable, stat.Files, stat.Ffree)
}

func (report *debugReport) phase(name string, started time.Time, sample SegmentSample, err error, outDir string) {
	if report == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	finished := time.Now()
	elapsed := sample.Duration
	if elapsed <= 0 {
		elapsed = finished.Sub(started)
	}
	report.write("phase name=%s started=%s finished=%s elapsed=%s telemetry_samples=%d highmem_jobs=%d r8_jobs=%d rust_jobs=%d java_jobs=%d kotlin_jobs=%d highmem_explicit=%t r8_explicit=%t rust_explicit=%t java_explicit=%t kotlin_explicit=%t pool_reason=%q analysis_memory_limit_bytes=%d analysis_memory_limit=%q analysis_gc_percent=%d mem_available_start_bytes=%d mem_available_end_bytes=%d min_mem_available_bytes=%d min_mem_available=%q swap_free_start_bytes=%d swap_free_end_bytes=%d min_swap_free_bytes=%d min_swap_free=%q swap_in_bytes=%d swap_out_bytes=%d swap_in=%q swap_out=%q major_faults=%d oom_kills=%d psi_some_avg10_peak=%.2f psi_some_avg60_peak=%.2f psi_some_avg300_peak=%.2f psi_full_avg10_peak=%.2f psi_full_avg60_peak=%.2f psi_full_avg300_peak=%.2f swap_in_peak_mib_min=%.1f swap_out_peak_mib_min=%.1f cpu_avg=%.1f cpu_peak=%.1f max_processes=%d max_tracked_rss_bytes=%d max_tracked_rss=%q max_r8=%d max_linker=%d max_javac=%d max_kotlinc=%d max_rustc=%d max_clang=%d result=%q\n",
		name, started.Format(time.RFC3339Nano), finished.Format(time.RFC3339Nano), elapsed.Round(time.Millisecond), sample.Samples,
		sample.HighmemJobs, sample.R8Jobs, sample.RustJobs, sample.JavaJobs, sample.KotlinJobs,
		sample.HighmemExplicit, sample.R8Explicit, sample.RustExplicit, sample.JavaExplicit, sample.KotlinExplicit, sample.PoolReason,
		sample.AnalysisLimit, formatBytes(sample.AnalysisLimit), sample.AnalysisGC,
		sample.InitialAvailable, sample.FinalAvailable, sample.MinimumAvailable, formatBytes(sample.MinimumAvailable),
		sample.InitialSwapFree, sample.FinalSwapFree, sample.MinimumSwapFree, formatBytes(sample.MinimumSwapFree),
		sample.SwapInBytes, sample.SwapOutBytes, formatBytes(int64(sample.SwapInBytes)), formatBytes(int64(sample.SwapOutBytes)),
		sample.MajorFaults, sample.OOMKills,
		sample.MaxPSISomeAvg10, sample.MaxPSISomeAvg60, sample.MaxPSISomeAvg300,
		sample.MaxPSIFullAvg10, sample.MaxPSIFullAvg60, sample.MaxPSIFullAvg300,
		sample.MaxSwapInRate*60/(1024*1024), sample.MaxSwapOutRate*60/(1024*1024),
		sample.AverageCPU, sample.MaxCPU, sample.MaxProcesses, sample.MaxTrackedRSS, formatBytes(sample.MaxTrackedRSS),
		sample.MaxR8, sample.MaxLinker, sample.MaxJavac,
		sample.MaxKotlinc, sample.MaxRustc, sample.MaxClang, result)
	for rank, process := range sample.TopRSS {
		report.write("phase_top_rss phase=%s rank=%d pid=%d name=%q task_type=%s rss_bytes=%d rss=%q at=%s\n",
			name, rank+1, process.PID, process.Name, process.TaskType, process.RSSBytes, formatBytes(process.RSSBytes),
			process.Timestamp.Format(time.RFC3339Nano))
	}
	for _, warning := range sample.Warnings {
		report.write("telemetry_warning phase=%s error=%q\n", name, warning)
	}
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
	if report == nil {
		return
	}
	report.system("finish", outDir)
	report.write("finished=%s\nelapsed=%s\n", time.Now().Format(time.RFC3339Nano), time.Since(report.started).Round(time.Millisecond))
	report.mu.Lock()
	defer report.mu.Unlock()
	if report.file == nil {
		return
	}
	_ = report.file.Sync()
	_ = report.file.Close()
	report.file = nil
}
