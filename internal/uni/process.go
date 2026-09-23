// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type commandRunner struct {
	top                     string
	useCgroup               bool
	useCcache               bool
	autoCcacheCompilerCheck bool
	autoCcacheDepend        bool
	autoCcacheFileClone     bool
	autoCcacheMaxSize       string
	requestedNinja          string
	phasedNinja             string
	scopePrefix             string
	baseEnv                 []string
	outDir                  string
	soongUIPath             string
	soongUIBinary           string
	trustOutput             bool
	assumeExistingNinja     string
	rustIncremental         bool
	rustCodegenUnits        int
	partialCompile          bool
	kotlinDaemon            bool
	incrementalAnalysis     bool
	criticalPathSource      string
	sisoPriorityTargets     []string
	admission               poolAdmission
	forceLocalNinja         bool
	kernelJobs              int
}

func phasedNinjaExecutor(requested string) string {
	if requested == "" || strings.EqualFold(requested, "siso") {
		return "ninja"
	}
	return requested
}

func preferLocalNinja(requested string, snapshot MemorySnapshot) bool {
	return strings.TrimSpace(requested) == "" && snapshot.Total > 0 && snapshot.Total < 48*gibibyte
}

func executorLabel(executor string) string {
	if executor == "" {
		return "default"
	}
	return executor
}

func forcePhasedNinja(mode, phase, assumeExisting string, forceLocal bool) bool {
	return mode == "--uni-ninja-mode" && (forceLocal || phase != "only" || assumeExisting != "")
}

func nestedKernelJobs(maxJobs int) int {
	return MaximumJobs(maxJobs)
}

func uniScopePrefix(outDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(outDir)))
	return fmt.Sprintf("uwu-uni-%x", digest[:6])
}

func overrideEnvironment(base []string, values ...string) []string {
	overrides := make(map[string]string, len(values))
	for _, value := range values {
		if index := strings.IndexByte(value, '='); index > 0 {
			overrides[value[:index]] = value
		}
	}
	result := make([]string, 0, len(base)+len(values))
	for _, value := range base {
		index := strings.IndexByte(value, '=')
		if index <= 0 {
			continue
		}
		if _, replaced := overrides[value[:index]]; !replaced {
			result = append(result, value)
		}
	}
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func environmentValue(environment []string, name string) (string, bool) {
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix), true
		}
	}
	return "", false
}

func positiveEnvironmentInt(environment []string, name string) (int, bool) {
	raw, set := environmentValue(environment, name)
	if !set {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value > 0
}

func environmentTrue(environment []string, name string) bool {
	value, set := environmentValue(environment, name)
	if set {
		value = strings.ToLower(strings.TrimSpace(value))
		return value == "1" || value == "true" || value == "y" || value == "yes"
	}
	return false
}

func rustCodegenUnitsForPool(environment []string) int {
	if value, set := positiveEnvironmentInt(environment, "SOONG_RUSTC_CODEGEN_UNITS"); set {
		return value
	}
	// Match transformSrctoCrate when Soong does not override rustc's defaults.
	if environmentTrue(environment, "SOONG_RUSTC_INCREMENTAL") {
		return 256
	}
	if variant, _ := environmentValue(environment, "TARGET_BUILD_VARIANT"); variant == "eng" {
		return 16
	}
	return 1
}

func newCommandRunner(ctx context.Context, top string, keyValues []string) (*commandRunner, error) {
	runner := &commandRunner{
		top:            top,
		requestedNinja: os.Getenv("SOONG_NINJA"),
		baseEnv:        os.Environ(),
		soongUIPath:    filepath.Join(top, "build", "soong", "soong_ui.bash"),
	}
	runner.baseEnv = overrideEnvironment(runner.baseEnv, keyValues...)
	runner.phasedNinja = phasedNinjaExecutor(runner.requestedNinja)
	outDir, err := outputDirectory(top)
	if err != nil {
		return nil, err
	}
	runner.soongUIBinary = filepath.Join(outDir, "soong_ui")
	runner.outDir = outDir
	if _, set := environmentValue(runner.baseEnv, "SOONG_USE_PARTIAL_COMPILE"); !set {
		runner.baseEnv = overrideEnvironment(runner.baseEnv, "SOONG_USE_PARTIAL_COMPILE=true")
	}
	if _, set := environmentValue(runner.baseEnv, "SOONG_KOTLIN_DAEMON"); !set {
		runner.baseEnv = overrideEnvironment(runner.baseEnv, "SOONG_KOTLIN_DAEMON=true")
	}
	if _, set := environmentValue(runner.baseEnv, "SOONG_INCREMENTAL_ANALYSIS"); !set {
		runner.baseEnv = overrideEnvironment(runner.baseEnv, "SOONG_INCREMENTAL_ANALYSIS=true")
	}
	runner.rustIncremental = environmentTrue(runner.baseEnv, "SOONG_RUSTC_INCREMENTAL")
	runner.rustCodegenUnits = rustCodegenUnitsForPool(runner.baseEnv)
	runner.partialCompile = environmentTrue(runner.baseEnv, "SOONG_USE_PARTIAL_COMPILE")
	runner.kotlinDaemon = environmentTrue(runner.baseEnv, "SOONG_KOTLIN_DAEMON")
	runner.incrementalAnalysis = environmentTrue(runner.baseEnv, "SOONG_INCREMENTAL_ANALYSIS")
	runner.criticalPathSource = "soong-hint"
	if _, err := os.Stat(filepath.Join(outDir, ".ninja_log")); err == nil {
		runner.criticalPathSource = "ninja-log"
	}
	runner.scopePrefix = uniScopePrefix(outDir)
	active, err := activeBuildStillRunning(outDir)
	if err != nil {
		return nil, fmt.Errorf("inspect active uni build: %w", err)
	}
	if active != "" {
		return nil, fmt.Errorf("an existing uni build is still running: %s", active)
	}
	useCcache, _ := environmentValue(runner.baseEnv, "USE_CCACHE")
	runner.useCcache = useCcache != "" && !strings.EqualFold(useCcache, "false")
	_, runnerCompilerCheckSet := environmentValue(runner.baseEnv, "CCACHE_COMPILERCHECK")
	runner.autoCcacheCompilerCheck = runner.useCcache && !runnerCompilerCheckSet
	_, runnerDependSet := environmentValue(runner.baseEnv, "CCACHE_DEPEND")
	_, runnerNoDependSet := environmentValue(runner.baseEnv, "CCACHE_NODEPEND")
	runner.autoCcacheDepend = runner.useCcache && !runnerDependSet && !runnerNoDependSet
	if runner.autoCcacheDepend {
		runner.baseEnv = overrideEnvironment(runner.baseEnv, "CCACHE_DEPEND=true")
	}
	_, runnerFileCloneSet := environmentValue(runner.baseEnv, "CCACHE_FILECLONE")
	runner.autoCcacheFileClone = runner.useCcache && !runnerFileCloneSet && canAutoEnableFileClone(top, outDir)
	_, runnerMaxSizeSet := environmentValue(runner.baseEnv, "CCACHE_MAXSIZE")
	if runner.useCcache && !runnerMaxSizeSet {
		runner.autoCcacheMaxSize = automaticCcacheMaxSize(outDir, runner.baseEnv)
		if runner.autoCcacheMaxSize != "" {
			runner.baseEnv = overrideEnvironment(runner.baseEnv,
				"CCACHE_MAXSIZE="+runner.autoCcacheMaxSize)
		}
	}
	if _, err := os.Stat(runner.soongUIPath); err != nil {
		return nil, err
	}
	runner.useCgroup = runner.probeCgroup(ctx)
	return runner, nil
}

func (runner *commandRunner) runReported(ctx context.Context, report *debugReport, summary *buildSummary, name, mode, phase, statePath string, args []string, maxJobs int) (SegmentSample, error) {
	return runner.runReportedAttempt(ctx, report, summary, name, mode, phase, statePath, args, maxJobs, 0)
}

func (runner *commandRunner) runReportedAttempt(ctx context.Context, report *debugReport, summary *buildSummary, name, mode, phase, statePath string, args []string, maxJobs, memoryRetries int) (SegmentSample, error) {
	tui := compactTUIFromContext(ctx)
	if tui != nil {
		tui.phaseStarted(name, maxJobs)
	}
	if report != nil {
		report.system(name+"-start", runner.outDir)
		report.event("command_start phase=%s mode=%s ninja_phase=%s jobs=%d arguments=%s", name, mode, phase, maxJobs, quoteArguments(args))
	}
	started := time.Now()
	var telemetrySink, liveTelemetrySink func(TelemetrySample)
	if report != nil {
		telemetrySink = func(sample TelemetrySample) {
			report.telemetry(name, sample)
		}
	}
	if tui != nil {
		liveTelemetrySink = tui.updateTelemetry
	}
	sample, err := runner.runWithTelemetry(ctx, mode, phase, statePath, args, maxJobs, memoryRetries, telemetrySink, liveTelemetrySink)
	sample.Phase = name
	sample.Duration = time.Since(started)
	summary.add(sample)
	if report != nil {
		report.phase(name, started, sample, err, runner.outDir)
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		report.event("command_end phase=%s elapsed=%s result=%q", name, sample.Duration.Round(time.Millisecond), result)
	}
	if tui != nil {
		tui.phaseFinished(name, err)
	}
	if jobs, retry := memoryRetryJobs(err, ctx.Err(), maxJobs, memoryRetries); retry {
		nextRetry := memoryRetries + 1
		fmt.Printf("uni: sustained memory pressure; waiting before -j%d retry %d/2\n", jobs, nextRetry)
		if report != nil {
			report.event("memory_recovery_wait retry=%d jobs=%d timeout=%s", nextRetry, jobs, memoryRecoveryTimeout)
		}
		waited, recovered := waitForMemoryRecovery(ctx, memoryRecoveryTimeout)
		if report != nil {
			report.event("memory_recovery_wait_end retry=%d jobs=%d elapsed=%s recovered=%t", nextRetry, jobs, waited.Round(time.Millisecond), recovered)
		}
		if !recovered {
			if ctx.Err() != nil {
				return sample, ctx.Err()
			}
			return sample, fmt.Errorf("%w: pressure remained high for %s", errMemoryPressure, waited.Round(time.Second))
		}
		fmt.Printf("uni: memory pressure recovered after %s; resuming completed outputs\n", waited.Round(time.Second))
		return runner.runReportedAttempt(ctx, report, summary, name, mode, phase, statePath,
			replaceParallelArgs(args, jobs), jobs, nextRetry)
	}
	return sample, err
}

func (runner *commandRunner) probeCgroup(ctx context.Context) bool {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, "systemd-run", "--user", "--scope", "--quiet", "true")
	return cmd.Run() == nil
}

func (runner *commandRunner) run(ctx context.Context, mode, phase, statePath string, args []string, maxJobs int) (SegmentSample, error) {
	return runner.runWithTelemetry(ctx, mode, phase, statePath, args, maxJobs, 0, nil, nil)
}

func (runner *commandRunner) runWithTelemetry(ctx context.Context, mode, phase, statePath string, args []string, maxJobs, memoryRetries int, telemetrySink, liveTelemetrySink func(TelemetrySample)) (SegmentSample, error) {
	snapshot, err := ReadMemorySnapshot()
	var telemetryWarnings []string
	if err != nil {
		telemetryWarnings = append(telemetryWarnings, fmt.Sprintf("read initial memory telemetry: %v", err))
		snapshot = MemorySnapshot{}
	}
	command := runner.soongUIPath
	if mode == "--uni-ninja-mode" {
		if info, err := os.Stat(runner.soongUIBinary); err == nil && info.Mode()&0111 != 0 {
			command = runner.soongUIBinary
		}
	}
	commandArgs := append([]string{mode}, args...)
	scopeUnit := ""
	if runner.useCgroup {
		scopeUnit = fmt.Sprintf("%s-%d-%d.scope", runner.scopePrefix, os.Getpid(), time.Now().UnixNano())
		commandArgs = append([]string{"--user", "--scope", "--quiet", "--collect", "--unit=" + scopeUnit,
			command}, commandArgs...)
		command = "systemd-run"
	}

	cmd := exec.Command(command, commandArgs...)
	cmd.Dir = runner.top
	if compactTUIFromContext(ctx) == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	overrides := []string{
		"TOP=" + runner.top,
		"ORIGINAL_PWD=" + runner.top,
		"NETWORK_FILE_SYSTEM_TYPE=local",
		"_SOONG_INTERNAL_NO_FINDER=true",
		"UNI_STATE_FILE=" + statePath,
		"UNI_R8_MODULES_FILE=" + filepath.Join(filepath.Dir(statePath), "soong_r8_modules.txt"),
		"UNI_NINJA_PHASE=" + phase,
	}
	analysisMemoryLimit := int64(0)
	analysisGCPercent := 0
	if mode == "--uni-prepare-mode" {
		if value, set := environmentValue(runner.baseEnv, "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES"); set {
			analysisMemoryLimit, _ = strconv.ParseInt(value, 10, 64)
		} else {
			analysisMemoryLimit = AnalysisMemoryLimit(snapshot.Total, snapshot.Available)
			if analysisMemoryLimit > 0 {
				overrides = append(overrides, "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES="+strconv.FormatInt(analysisMemoryLimit, 10))
			}
		}
		if value, set := environmentValue(runner.baseEnv, "SOONG_ANALYSIS_GC_PERCENT"); set {
			analysisGCPercent, _ = strconv.Atoi(value)
		} else {
			analysisGCPercent = 100
			overrides = append(overrides, "SOONG_ANALYSIS_GC_PERCENT="+strconv.Itoa(analysisGCPercent))
		}
		fmt.Printf("uni: Android.bp analysis memory limit %s, GOGC=%d\n",
			formatBytes(analysisMemoryLimit), analysisGCPercent)
	}
	if forcePhasedNinja(mode, phase, runner.assumeExistingNinja, runner.forceLocalNinja) {
		overrides = append(overrides, "SOONG_NINJA="+runner.phasedNinja)
		if runner.phasedNinja == "ninja" {
			overrides = append(overrides, "NO_ABFS=true")
		}
	}
	if mode == "--uni-ninja-mode" {
		if runner.kernelJobs > 0 {
			if _, set := environmentValue(runner.baseEnv, "UNI_KERNEL_JOBS"); !set {
				overrides = append(overrides, "UNI_KERNEL_JOBS="+strconv.Itoa(runner.kernelJobs))
			}
		}
		if phase == "only" && len(runner.sisoPriorityTargets) > 0 {
			priorityTargets, marshalErr := json.Marshal(runner.sisoPriorityTargets)
			if marshalErr != nil {
				return SegmentSample{}, fmt.Errorf("encode Siso priority targets: %w", marshalErr)
			}
			overrides = append(overrides, "UNI_SISO_PRIORITY_TARGETS="+string(priorityTargets))
		}
		if _, set := os.LookupEnv("SOONG_UI_TABLE_HEIGHT"); !set {
			overrides = append(overrides, "SOONG_UI_TABLE_HEIGHT=4")
		}
		if err := prepareNinjaState(runner.outDir, runner.trustOutput); err != nil {
			return SegmentSample{}, fmt.Errorf("prepare Ninja recovery state: %w", err)
		}
		if runner.assumeExistingNinja != "" {
			overrides = append(overrides,
				"UNI_NINJA_BIN="+runner.assumeExistingNinja,
				"UNI_ASSUME_EXISTING=true")
		}
	}
	if runner.autoCcacheCompilerCheck {
		overrides = append(overrides, "CCACHE_COMPILERCHECK=mtime")
	}
	if runner.autoCcacheFileClone {
		overrides = append(overrides, "CCACHE_FILECLONE=true")
	}
	pools := runner.admission.decide(maxJobs, snapshot, runner.baseEnv, runner.rustCodegenUnits)
	pools = pools.forMemoryRetry(memoryRetries)
	highmemJobs, r8Jobs, rustJobs := pools.highmem, pools.r8, pools.rust
	javaJobs, kotlinJobs := pools.java, pools.kotlin
	if !pools.highmemExplicit {
		overrides = append(overrides, "NINJA_HIGHMEM_NUM_JOBS="+strconv.Itoa(highmemJobs))
	}
	if !pools.r8Explicit {
		overrides = append(overrides, "NINJA_UNI_R8_NUM_JOBS="+strconv.Itoa(r8Jobs))
	}
	if !pools.rustExplicit {
		overrides = append(overrides, "NINJA_UNI_RUST_NUM_JOBS="+strconv.Itoa(rustJobs))
	}
	if !pools.javaExplicit {
		overrides = append(overrides, "NINJA_UNI_JAVA_NUM_JOBS="+strconv.Itoa(javaJobs))
	}
	if !pools.kotlinExplicit {
		overrides = append(overrides, "NINJA_UNI_KOTLIN_NUM_JOBS="+strconv.Itoa(kotlinJobs))
	}
	fmt.Printf("uni: pools high-memory=%d R8=%d Rust=%d Java=%d Kotlin=%d, available %s\n",
		highmemJobs, r8Jobs, rustJobs, javaJobs, kotlinJobs, formatBytes(snapshot.Available))
	environment := overrideEnvironment(runner.baseEnv, overrides...)
	cmd.Env = environment

	recoveryMarked := false
	if mode == "--uni-ninja-mode" {
		if err := markNinjaRecoveryRequired(runner.outDir); err != nil {
			return SegmentSample{}, fmt.Errorf("mark Ninja recovery state: %w", err)
		}
		recoveryMarked = true
	}
	if err := cmd.Start(); err != nil {
		if recoveryMarked {
			_ = clearNinjaRecoveryRequired(runner.outDir)
		}
		return SegmentSample{}, err
	}
	rootIdentity, identityErr := readProcessIdentity(cmd.Process.Pid)
	if identityErr != nil {
		terminateProcessTree(cmd.Process.Pid, scopeUnit)
		_ = cmd.Wait()
		if recoveryMarked {
			if checkpointErr := checkpointNinjaState(runner.outDir, true, runner.trustOutput); checkpointErr == nil {
				_ = clearNinjaRecoveryRequired(runner.outDir)
			}
		}
		return SegmentSample{}, fmt.Errorf("identify active build: %w", identityErr)
	}
	lease, leaseErr := writeActiveBuildLeaseForProcess(runner.outDir, rootIdentity, scopeUnit)
	if leaseErr != nil {
		terminateBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
		_ = cmd.Wait()
		if recoveryMarked {
			if checkpointErr := checkpointNinjaState(runner.outDir, true, runner.trustOutput); checkpointErr == nil {
				_ = clearNinjaRecoveryRequired(runner.outDir)
			}
		}
		return SegmentSample{}, fmt.Errorf("record active build: %w", leaseErr)
	}
	monitor := startMemoryMonitor(rootIdentity, runner.outDir, telemetrySink, liveTelemetrySink)
	done := make(chan struct{})
	cancelFinished := make(chan struct{})
	pressureTriggered := false
	go func() {
		defer close(cancelFinished)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var pressure memoryPressureGuard
		for {
			select {
			case <-ctx.Done():
				interruptBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
				return
			case <-done:
				if ctx.Err() != nil {
					interruptBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
				}
				return
			case now := <-ticker.C:
				if mode != "--uni-ninja-mode" {
					continue
				}
				memory, memoryErr := ReadMemorySnapshot()
				psi, psiErr := readMemoryPSI()
				if memoryErr != nil || psiErr != nil {
					pressure = memoryPressureGuard{}
					continue
				}
				linkerHeavy := false
				if memory.Total > 0 && memory.Available < 2*max(3*gibibyte, memory.Total/8) && psi.full.avg10 >= 35 {
					linkers := 0
					threshold := max(4, (maxJobs+2)/3)
					for _, identity := range snapshotProcessTreeForRoot(rootIdentity) {
						process, err := readProcessTelemetry(identity)
						if err == nil && process.taskType == "linker" {
							linkers++
							if linkers >= threshold {
								linkerHeavy = true
								break
							}
						}
					}
				}
				if pressure.observe(now, memory, psi.full.avg10, linkerHeavy) {
					pressureTriggered = true
					terminateBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
					return
				}
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	<-cancelFinished
	leaseErr = clearActiveBuildLease(runner.outDir, lease.Token)
	sample := monitor.finish()
	sample.Warnings = append(telemetryWarnings, sample.Warnings...)
	sample.HighmemJobs = highmemJobs
	sample.R8Jobs = r8Jobs
	sample.RustJobs = rustJobs
	sample.JavaJobs = javaJobs
	sample.KotlinJobs = kotlinJobs
	sample.HighmemExplicit = pools.highmemExplicit
	sample.R8Explicit = pools.r8Explicit
	sample.RustExplicit = pools.rustExplicit
	sample.JavaExplicit = pools.javaExplicit
	sample.KotlinExplicit = pools.kotlinExplicit
	sample.PoolReason = pools.reason
	sample.AnalysisLimit = analysisMemoryLimit
	sample.AnalysisGC = analysisGCPercent
	runner.admission.observe(sample)
	for _, warning := range sample.Warnings {
		fmt.Fprintf(os.Stderr, "uni: telemetry warning: %s\n", warning)
	}
	var checkpointErr error
	if mode == "--uni-ninja-mode" {
		checkpointErr = checkpointNinjaState(runner.outDir, err != nil, runner.trustOutput)
		if checkpointErr == nil {
			checkpointErr = clearNinjaRecoveryRequired(runner.outDir)
		}
		if checkpointErr != nil {
			if err == nil {
				return sample, fmt.Errorf("checkpoint Ninja recovery state: %w", checkpointErr)
			}
			fmt.Fprintf(os.Stderr, "uni: checkpoint Ninja recovery state: %v\n", checkpointErr)
		}
	}
	if leaseErr != nil && err == nil {
		return sample, fmt.Errorf("clear active build: %w", leaseErr)
	}
	if pressureTriggered && err != nil && ctx.Err() == nil && checkpointErr == nil && leaseErr == nil {
		if len(runningUniProcesses(runner.outDir)) != 0 {
			return sample, fmt.Errorf("memory recovery stopped: build processes remain")
		}
		return sample, errMemoryPressure
	}
	if err != nil {
		if ctx.Err() != nil {
			return sample, ctx.Err()
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return sample, fmt.Errorf("%s terminated by signal %s", mode, status.Signal())
			}
			return sample, fmt.Errorf("%s exited with status %d", mode, exitError.ExitCode())
		}
		return sample, err
	}
	return sample, nil
}

func runSystemctl(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Run()
}

func terminateProcessIdentityTree(root processIdentity, scopeUnit string) {
	processes := signalProcessTreeIdentity(root, syscall.SIGTERM)
	if scopeUnit != "" {
		runSystemctl("kill", "--kill-whom=all", "--signal=SIGTERM", scopeUnit)
		runSystemctl("stop", "--no-block", scopeUnit)
	}
	exited := waitForProcessTreeExit(root.PID, processes, 2*time.Second)
	if exited {
		return
	}
	processes = mergeProcessIdentities(processes, snapshotProcessTreeForRoot(root))
	if sameProcess(root) {
		_ = syscall.Kill(-root.PID, syscall.SIGKILL)
	}
	signalProcessIdentities(processes, syscall.SIGKILL)
	if scopeUnit != "" {
		runSystemctl("kill", "--kill-whom=all", "--signal=SIGKILL", scopeUnit)
		runSystemctl("stop", "--no-block", scopeUnit)
	}
	waitForProcessTreeExit(root.PID, processes, 2*time.Second)
}

func terminateProcessTree(pid int, scopeUnit string) {
	root, err := readProcessIdentity(pid)
	if err != nil {
		if scopeUnit != "" {
			runSystemctl("kill", "--kill-whom=all", "--signal=SIGKILL", scopeUnit)
			runSystemctl("stop", "--no-block", scopeUnit)
		}
		return
	}
	terminateProcessIdentityTree(root, scopeUnit)
}

func terminateBuildProcessTree(root processIdentity, scopeUnit, outDir string) {
	terminateProcessIdentityTree(root, scopeUnit)
	processes := runningUniProcesses(outDir)
	signalProcessIdentities(processes, syscall.SIGTERM)
	waitForProcessIdentitiesExit(processes, 500*time.Millisecond)
	processes = runningUniProcesses(outDir)
	if len(processes) == 0 {
		return
	}
	signalProcessIdentities(processes, syscall.SIGKILL)
	waitForProcessIdentitiesExit(processes, 2*time.Second)
}

func interruptBuildProcessTree(root processIdentity, scopeUnit, outDir string) {
	processes := signalProcessTreeIdentity(root, syscall.SIGINT)
	if waitForProcessTreeExit(root.PID, processes, 750*time.Millisecond) &&
		len(runningUniProcesses(outDir)) == 0 {
		return
	}
	terminateBuildProcessTree(root, scopeUnit, outDir)
	signalProcessIdentities(processes, syscall.SIGKILL)
	waitForProcessIdentitiesExit(processes, 2*time.Second)
}

func waitForProcessGroupExit(pid int, timeout time.Duration) bool {
	return waitForProcessTreeExit(pid, nil, timeout)
}

func canAutoEnableFileClone(top, outDir string) bool {
	cacheDir := os.Getenv("CCACHE_DIR")
	if cacheDir == "" {
		userCacheDir, err := os.UserCacheDir()
		if err != nil {
			return false
		}
		cacheDir = filepath.Join(userCacheDir, "ccache")
	} else if !filepath.IsAbs(cacheDir) {
		cacheDir = filepath.Join(top, cacheDir)
	}
	var outStat, cacheStat syscall.Stat_t
	if syscall.Stat(outDir, &outStat) != nil || syscall.Stat(cacheDir, &cacheStat) != nil || outStat.Dev != cacheStat.Dev {
		return false
	}
	var fileSystem syscall.Statfs_t
	if syscall.Statfs(outDir, &fileSystem) != nil || uint64(fileSystem.Type) != 0x9123683e {
		return false
	}
	free := int64(fileSystem.Bavail) * int64(fileSystem.Bsize)
	return free >= 32*gibibyte
}

func ccacheMaxSizeForDisk(currentSize, configuredMax, diskFree int64) int64 {
	const (
		maximum = 40 * gibibyte
		reserve = 40 * gibibyte
	)
	if configuredMax >= maximum {
		return 0
	}
	growth := max(int64(0), maximum-currentSize)
	if diskFree-growth < reserve {
		return 0
	}
	return maximum
}

func automaticCcacheMaxSize(outDir string, environment []string) string {
	stats, err := readCcacheStats(environment)
	if err != nil {
		return ""
	}
	diskFree, err := readDiskAvailable(outDir)
	if err != nil {
		return ""
	}
	currentSize := int64(stats["cache_size_kibibyte"]) * 1024
	configuredMax := int64(stats["max_cache_size_kibibyte"]) * 1024
	target := ccacheMaxSizeForDisk(currentSize, configuredMax, diskFree)
	if target == 0 {
		return ""
	}
	return strconv.FormatInt(target/gibibyte, 10) + "G"
}
