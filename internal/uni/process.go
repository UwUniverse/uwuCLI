// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"crypto/sha256"
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
	autoCcacheFileClone     bool
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
	partialCompile          bool
	kotlinDaemon            bool
	incrementalAnalysis     bool
	criticalPathSource      string
}

func phasedNinjaExecutor(requested string) string {
	if requested == "" || strings.EqualFold(requested, "siso") {
		return "ninja"
	}
	return requested
}

func executorLabel(executor string) string {
	if executor == "" {
		return "default"
	}
	return executor
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
	useCcache := os.Getenv("USE_CCACHE")
	runner.useCcache = useCcache != "" && !strings.EqualFold(useCcache, "false")
	_, runnerCompilerCheckSet := os.LookupEnv("CCACHE_COMPILERCHECK")
	runner.autoCcacheCompilerCheck = runner.useCcache && !runnerCompilerCheckSet
	_, runnerFileCloneSet := os.LookupEnv("CCACHE_FILECLONE")
	runner.autoCcacheFileClone = runner.useCcache && !runnerFileCloneSet && canAutoEnableFileClone(top, outDir)
	if _, err := os.Stat(runner.soongUIPath); err != nil {
		return nil, err
	}
	runner.useCgroup = runner.probeCgroup(ctx)
	return runner, nil
}

func (runner *commandRunner) disableIncrementalAnalysis() {
	runner.baseEnv = overrideEnvironment(runner.baseEnv, "SOONG_INCREMENTAL_ANALYSIS=false")
	runner.incrementalAnalysis = false
}

func (runner *commandRunner) runReported(ctx context.Context, report *debugReport, summary *buildSummary, name, mode, phase, statePath string, args []string, maxJobs int) (SegmentSample, error) {
	if report != nil {
		report.system(name+"-start", runner.outDir)
	}
	started := time.Now()
	sample, err := runner.run(ctx, mode, phase, statePath, args, maxJobs)
	summary.add(sample)
	if report != nil {
		report.phase(name, started, sample, err, runner.outDir)
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
	snapshot, err := ReadMemorySnapshot()
	if err != nil {
		return SegmentSample{}, err
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
	cmd.Stdin = os.Stdin
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
	if mode == "--uni-prepare-mode" {
		if _, set := os.LookupEnv("SOONG_ANALYSIS_MEMORY_LIMIT_BYTES"); !set {
			limit := AnalysisMemoryLimit(snapshot.Total)
			if limit > 0 {
				overrides = append(overrides, "SOONG_ANALYSIS_MEMORY_LIMIT_BYTES="+strconv.FormatInt(limit, 10))
			}
		}
		if _, set := os.LookupEnv("SOONG_ANALYSIS_GC_PERCENT"); !set {
			overrides = append(overrides, "SOONG_ANALYSIS_GC_PERCENT=200")
		}
	}
	if mode == "--uni-ninja-mode" && (phase != "only" || runner.assumeExistingNinja != "") {
		overrides = append(overrides, "SOONG_NINJA="+runner.phasedNinja)
		if runner.phasedNinja == "ninja" {
			overrides = append(overrides, "NO_ABFS=true")
		}
	}
	if mode == "--uni-ninja-mode" {
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
	highmemJobs := HighmemJobs(maxJobs, snapshot)
	if value, valid := positiveEnvironmentInt(runner.baseEnv, "NINJA_HIGHMEM_NUM_JOBS"); valid {
		highmemJobs = value
	} else {
		overrides = append(overrides, "NINJA_HIGHMEM_NUM_JOBS="+strconv.Itoa(highmemJobs))
	}
	r8Jobs := highmemJobs
	if value, valid := positiveEnvironmentInt(runner.baseEnv, "NINJA_UNI_R8_NUM_JOBS"); valid {
		r8Jobs = value
	} else {
		overrides = append(overrides, "NINJA_UNI_R8_NUM_JOBS="+strconv.Itoa(r8Jobs))
	}
	javaJobs := JavaJobs(maxJobs, snapshot)
	if value, valid := positiveEnvironmentInt(runner.baseEnv, "NINJA_UNI_JAVA_NUM_JOBS"); valid {
		javaJobs = value
	} else {
		overrides = append(overrides, "NINJA_UNI_JAVA_NUM_JOBS="+strconv.Itoa(javaJobs))
	}
	kotlinJobs := KotlinJobs(maxJobs, snapshot)
	if value, valid := positiveEnvironmentInt(runner.baseEnv, "NINJA_UNI_KOTLIN_NUM_JOBS"); valid {
		kotlinJobs = value
	} else {
		overrides = append(overrides, "NINJA_UNI_KOTLIN_NUM_JOBS="+strconv.Itoa(kotlinJobs))
	}
	fmt.Printf("uni: pools high-memory=%d R8=%d Java=%d Kotlin=%d, available %s\n",
		highmemJobs, r8Jobs, javaJobs, kotlinJobs, formatBytes(snapshot.Available))
	environment := overrideEnvironment(runner.baseEnv, overrides...)
	cmd.Env = environment

	monitor := startMemoryMonitor()
	recoveryMarked := false
	if mode == "--uni-ninja-mode" {
		if err := markNinjaRecoveryRequired(runner.outDir); err != nil {
			monitor.finish()
			return SegmentSample{}, fmt.Errorf("mark Ninja recovery state: %w", err)
		}
		recoveryMarked = true
	}
	if err := cmd.Start(); err != nil {
		if recoveryMarked {
			_ = clearNinjaRecoveryRequired(runner.outDir)
		}
		monitor.finish()
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
		monitor.finish()
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
		monitor.finish()
		return SegmentSample{}, fmt.Errorf("record active build: %w", leaseErr)
	}
	done := make(chan struct{})
	cancelFinished := make(chan struct{})
	go func() {
		defer close(cancelFinished)
		select {
		case <-ctx.Done():
			terminateBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
		case <-done:
			if ctx.Err() != nil {
				terminateBuildProcessTree(rootIdentity, scopeUnit, runner.outDir)
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	<-cancelFinished
	leaseErr = clearActiveBuildLease(runner.outDir, lease.Token)
	sample := monitor.finish()
	sample.HighmemJobs = highmemJobs
	sample.R8Jobs = r8Jobs
	sample.JavaJobs = javaJobs
	sample.KotlinJobs = kotlinJobs
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
	if exited && scopeUnit == "" {
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
	if waitForProcessIdentitiesExit(processes, 500*time.Millisecond) {
		return
	}
	processes = mergeProcessIdentities(processes, runningUniProcesses(outDir))
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
