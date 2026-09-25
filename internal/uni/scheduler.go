// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func findTop() (string, error) {
	if top := os.Getenv("TOP"); top != "" {
		absolute, err := filepath.Abs(top)
		if err == nil {
			if _, statErr := os.Stat(filepath.Join(absolute, "build", "soong", "root.bp")); statErr == nil {
				return absolute, nil
			}
		}
	}
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "build", "soong", "root.bp")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("run lunch before uni, or run uni inside an Android source tree")
		}
		directory = parent
	}
}

func outputDirectory(top string) (string, error) {
	outDir := os.Getenv("OUT_DIR")
	if outDir == "" {
		outDir = "out"
	}
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(top, outDir)
	}
	return filepath.Abs(outDir)
}

func signingCheckProductOut(outDir, product string) string {
	if productOut := strings.TrimSpace(os.Getenv("ANDROID_PRODUCT_OUT")); productOut != "" {
		return filepath.Clean(productOut)
	}
	return filepath.Join(outDir, "target", "product", product)
}

type outputLock struct {
	file *os.File
}

func acquireOutputLock(outDir string) (*outputLock, error) {
	lockDir := filepath.Join(outDir, "uni")
	if err := os.MkdirAll(lockDir, 0777); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(lockDir, ".lock"), os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another uni process is using %s", outDir)
	}
	return &outputLock{file: file}, nil
}

func (lock *outputLock) close() {
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

func removeCompleted(targets []string, completed map[string]struct{}) []string {
	result := targets[:0]
	for _, target := range targets {
		if _, ok := completed[target]; !ok {
			result = append(result, target)
		}
	}
	return result
}

func finalNinjaTargets(state State, options Options, earlyKernel string) []string {
	targets := append([]string(nil), state.NinjaArgs...)
	if len(targets) == 0 && len(options.Targets) > 0 {
		targets = append(targets, options.Targets...)
	}
	if earlyKernel == "" {
		return targets
	}
	for _, target := range targets {
		if target == earlyKernel {
			return targets
		}
	}
	return append([]string{earlyKernel}, targets...)
}

type startupSchedule struct {
	targets      []string
	packageCount int
	historyCount int
	r8Count      int
	nonR8Count   int
}

func startupPackageLimit(jobs, packages int) int {
	if jobs < 1 || packages < 1 {
		return 0
	}
	return min(packages, max(4, jobs/2))
}

func startupPhaseJobs(jobs, kernelJobs int, hasKernel bool) int {
	if !hasKernel || kernelJobs <= 0 {
		return jobs
	}
	return max(1, jobs-kernelJobs+1)
}

func selectStartupSchedule(packages []string, weights map[string]float64, r8Modules map[string]struct{}, earlyKernel string, jobs int) startupSchedule {
	limit := startupPackageLimit(jobs, len(packages))
	schedule := startupSchedule{targets: make([]string, 0, limit+1)}
	seen := make(map[string]struct{}, limit+1)
	appendTarget := func(target string, historical bool) {
		if target == "" {
			return
		}
		if _, exists := seen[target]; exists {
			return
		}
		seen[target] = struct{}{}
		schedule.targets = append(schedule.targets, target)
		if target == earlyKernel {
			return
		}
		schedule.packageCount++
		if historical {
			schedule.historyCount++
		}
		if _, isR8 := r8Modules[target]; isR8 {
			schedule.r8Count++
		} else {
			schedule.nonR8Count++
		}
	}

	appendTarget(earlyKernel, false)
	for _, target := range LongPrimeTargets(packages, weights, limit) {
		appendTarget(target, true)
	}
	for _, target := range packages {
		if schedule.packageCount >= limit {
			break
		}
		if _, isR8 := r8Modules[target]; isR8 {
			appendTarget(target, false)
		}
	}
	return schedule
}

func constrainStartupForGraph(schedule startupSchedule, earlyKernel string, singleGraph bool) startupSchedule {
	if !singleGraph {
		return schedule
	}
	result := startupSchedule{}
	if earlyKernel != "" {
		result.targets = []string{earlyKernel}
	}
	return result
}

func printPlan(state State, packages []string, startup startupSchedule, r8Count, batchSize, startupJobs, finalJobs int, earlyKernel string) {
	segments := len(Batches(packages, batchSize))
	fmt.Printf("product: %s (%s-%s)\n", state.TargetProduct, state.TargetRelease, state.BuildVariant)
	fmt.Printf("graph: %s\n", state.CombinedNinja)
	fmt.Printf("packages: %d\n", len(packages))
	fmt.Printf("R8 modules: %d\n", r8Count)
	fmt.Printf("startup: %d package target(s), history=%d R8=%d non-R8=%d\n",
		startup.packageCount, startup.historyCount, startup.r8Count, startup.nonR8Count)
	if segments <= 1 {
		fmt.Printf("segments: startup + final graph\n")
	} else {
		fmt.Printf("segments: startup + %d package; final target joins the last segment\n", segments)
	}
	fmt.Printf("initial: %d target(s), -j%d; final: -j%d\n", len(startup.targets), startupJobs, finalJobs)
	if earlyKernel == "" {
		fmt.Printf("kernel in startup: false\n")
	} else {
		fmt.Printf("kernel in startup: %s\n", earlyKernel)
	}
}

func printSinglePhasePlan(state State, jobs int) {
	fmt.Printf("product: %s (%s-%s)\n", state.TargetProduct, state.TargetRelease, state.BuildVariant)
	fmt.Printf("graph: %s\n", state.CombinedNinja)
	fmt.Printf("targets: %d\n", len(state.NinjaArgs))
	fmt.Printf("single Ninja phase: -j%d\n", jobs)
}

func scheduledBatchSize(options Options, targets int, snapshot MemorySnapshot) int {
	if options.Static {
		if targets > 0 {
			return min(options.BatchSize, targets)
		}
		return options.BatchSize
	}
	return InitialBatchSize(options.BatchSize, targets, snapshot)
}

func scheduledR8Limit(packages []string, r8Modules map[string]struct{}, batchSize int) int {
	segments := max(1, len(Batches(packages, batchSize)))
	count := R8TargetCount(packages, r8Modules)
	if count == 0 {
		return 0
	}
	return max(1, (count+segments-1)/segments)
}

func takeBatchWithR8Limit(targets []string, size int, r8Modules map[string]struct{}, r8Limit int) []string {
	if size <= 0 || len(targets) == 0 {
		return nil
	}
	batch := make([]string, 0, min(size, len(targets)))
	r8Count := 0
	for _, target := range targets {
		_, isR8 := r8Modules[target]
		if isR8 && r8Limit > 0 && r8Count >= r8Limit {
			continue
		}
		batch = append(batch, target)
		if isR8 {
			r8Count++
		}
		if len(batch) == size {
			break
		}
	}
	if len(batch) == 0 {
		batch = append(batch, targets[0])
	}
	return batch
}

func useSingleGraph(packages []string, batchSize int) bool {
	return len(Batches(packages, batchSize)) <= 1
}

func memorySnapshotOrWarning(report *debugReport, label string) MemorySnapshot {
	snapshot, err := ReadMemorySnapshot()
	if err == nil {
		return snapshot
	}
	fmt.Fprintf(os.Stderr, "uni: telemetry warning: %s: %v\n", label, err)
	report.event("telemetry_warning label=%s error=%q", label, err)
	return MemorySnapshot{}
}

func formatBuildDuration(elapsed time.Duration) string {
	total := int64(elapsed / time.Second)
	hours := total / 3600
	minutes := total % 3600 / 60
	seconds := total % 60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d (hh:mm:ss)", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%02d:%02d (mm:ss)", minutes, seconds)
	}
	return fmt.Sprintf("%d seconds", seconds)
}

type buildSummary struct {
	phases           int
	minimumAvailable int64
	swapOutBytes     uint64
	samples          []SegmentSample
}

func (summary *buildSummary) add(sample SegmentSample) {
	if summary == nil {
		return
	}
	summary.phases++
	if sample.MinimumAvailable > 0 &&
		(summary.minimumAvailable == 0 || sample.MinimumAvailable < summary.minimumAvailable) {
		summary.minimumAvailable = sample.MinimumAvailable
	}
	summary.swapOutBytes += sample.SwapOutBytes
	summary.samples = append(summary.samples, sample)
}

func formatBuildComplete(elapsed time.Duration) string {
	return fmt.Sprintf("#### build completed successfully (%s) ####", formatBuildDuration(elapsed))
}

func formatBuildSummary(summary buildSummary) string {
	return fmt.Sprintf("uni: phases=%d, min available=%s, swap-out=%s",
		summary.phases, formatBytes(summary.minimumAvailable), formatBytes(int64(summary.swapOutBytes)))
}

func formatBuildOutput(state State) string {
	for _, target := range state.NinjaArgs {
		if target == "otapackage" {
			return "uni: package=" + filepath.Join(state.ProductOut, state.TargetProduct+"-ota.zip")
		}
	}
	return "uni: output=" + state.ProductOut
}

func printBuildComplete(started time.Time, summary buildSummary, state State) {
	fmt.Println()
	fmt.Println(formatBuildSummary(summary))
	fmt.Println(formatBuildOutput(state))
	fmt.Printf("\033[0;32m%s\033[0m\n", formatBuildComplete(time.Since(started)))
}

func Run(ctx context.Context, options Options) error {
	started := time.Now()
	var summary buildSummary
	top, err := findTop()
	if err != nil {
		return err
	}
	if options.InitSigningKeys != "" {
		keysDir, err := initializeSigningKeys(ctx, top, options.InitSigningKeys)
		if err != nil {
			return err
		}
		fmt.Printf("uni: signing keys initialized: %s\n", keysDir)
		return nil
	}
	if options.SignKeys != "" {
		if _, err := validateSigningKeysDirectory(options.SignKeys); err != nil {
			return err
		}
	}
	if options.CleanLogs {
		outDir, err := outputDirectory(top)
		if err != nil {
			return err
		}
		lock, err := acquireOutputLock(outDir)
		if err != nil {
			return err
		}
		defer lock.close()
		cleaned, err := cleanBuildLogs(outDir)
		if err != nil {
			return fmt.Errorf("clean build logs: %w", err)
		}
		fmt.Printf("uni: removed %d log file(s), %s\n", cleaned.files, formatBytes(cleaned.bytes))
		return nil
	}
	if options.DevAutoSet {
		if err := saveDevAutoSetting(top, options.DevAuto); err != nil {
			return fmt.Errorf("save automatic R8 index setting: %w", err)
		}
	} else {
		options.DevAuto, err = loadDevAutoSetting(top)
		if err != nil {
			return fmt.Errorf("load automatic R8 index setting: %w", err)
		}
	}
	product := os.Getenv("TARGET_PRODUCT")
	if product == "" {
		return fmt.Errorf("TARGET_PRODUCT is empty; run lunch first")
	}
	outDir, err := outputDirectory(top)
	if err != nil {
		return err
	}
	lock, err := acquireOutputLock(outDir)
	if err != nil {
		return err
	}
	defer lock.close()
	var report *debugReport
	if options.Debug {
		report, err = newDebugReport(outDir, product, options.RawArgs)
		if err != nil {
			return fmt.Errorf("create debug report: %w", err)
		}
		fmt.Printf("uni: debug report: %s\n", report.path)
		report.event("paths source=%s out=%s", top, outDir)
		report.event("source_revision uwuCLI=%s soong=%s blueprint=%s",
			sourceRevision(filepath.Join(top, "uwuCLI")),
			sourceRevision(filepath.Join(top, "build", "soong")),
			sourceRevision(filepath.Join(top, "build", "blueprint")))
		defer report.close(outDir)
	}
	if options.SignCheck {
		state := State{
			TargetProduct: product,
			ProductOut:    signingCheckProductOut(outDir, product),
		}
		result, err := runSigning(ctx, top, outDir, state, options)
		if err != nil {
			return fmt.Errorf("signing check: %w", err)
		}
		report.event("signing check=true source=%s target_files=%s ota=%s checksum=%s", result.SourceTargetFiles, result.SignedTargetFiles, result.SignedOTA, result.Checksum)
		fmt.Printf("uni: signing check passed: %s\n", result.SignedOTA)
		return nil
	}
	startupCleanup := terminateResidualBuildProcesses(outDir)
	report.event("process_cleanup when=start found=%d term_sent=%d kill_sent=%d remaining=%d",
		startupCleanup.Found, startupCleanup.TermSent, startupCleanup.KillSent, startupCleanup.Remaining)
	if startupCleanup.Remaining > 0 {
		return fmt.Errorf("%d residual uni build process(es) survived cleanup", startupCleanup.Remaining)
	}
	report.event("options jobs=%d load_set=%t load=%.2f batch=%d static=%t plan=%t debug=%t dev=%t dev_auto=%t trust_output=%t assume_existing=%t force_reuse=%t full_build=%t dist=%t signing=%t targets=%d key_values=%d",
		options.MaxJobs, options.LoadSet, options.LoadAverage, options.BatchSize, options.Static, options.Plan,
		options.Debug, options.Dev, options.DevAuto, options.TrustOutput, options.AssumeExisting,
		options.ForceReuse, options.FullBuild, options.Dist, options.SignKeys != "", len(options.Targets), len(options.KeyValues))
	r8Mode := R8IndexFast
	r8IndexMode := "fast"
	if options.Dev {
		r8Mode = R8IndexFull
		r8IndexMode = "full"
	} else if options.DevAuto {
		r8Mode = R8IndexAuto
		r8IndexMode = "auto"
	}
	report.event("r8_index mode=%s", r8IndexMode)

	stateDir := filepath.Join(outDir, "uni", product)
	statePath := filepath.Join(stateDir, "state.json")
	runner, err := newCommandRunner(ctx, top, options.KeyValues)
	if err != nil {
		return err
	}
	defer func() {
		cleanup := terminateResidualBuildProcesses(outDir)
		report.event("process_cleanup when=finish found=%d term_sent=%d kill_sent=%d remaining=%d",
			cleanup.Found, cleanup.TermSent, cleanup.KillSent, cleanup.Remaining)
	}()
	if runner.useCcache {
		report.ccache("start", runner.baseEnv)
		defer report.ccache("finish", runner.baseEnv)
	}
	snapshot := memorySnapshotOrWarning(report, "initial-memory")
	autoLocalNinja := preferLocalNinja(runner.requestedNinja, snapshot)
	runner.forceLocalNinja = autoLocalNinja
	for _, name := range []string{
		"NINJA_HIGHMEM_NUM_JOBS", "NINJA_UNI_R8_NUM_JOBS", "NINJA_UNI_RUST_NUM_JOBS",
		"NINJA_UNI_JAVA_NUM_JOBS", "NINJA_UNI_KOTLIN_NUM_JOBS",
	} {
		value, set := environmentValue(runner.baseEnv, name)
		if !set {
			value = "<auto>"
		}
		report.event("pool_setting name=%s value=%q", name, value)
	}
	report.event("optimization rust_incremental=%t rust_codegen_units=%d partial_compile=%t kotlin_incremental_client=%t kotlin_daemon=%t critical_path=%s soong_incremental_analysis=%t",
		runner.rustIncremental, runner.rustCodegenUnits, runner.partialCompile, runner.partialCompile, runner.kotlinDaemon,
		runner.criticalPathSource, runner.incrementalAnalysis)
	runner.trustOutput = options.TrustOutput
	if options.AssumeExisting {
		runner.assumeExistingNinja, err = ensureAssumeExistingNinja(top, outDir)
		if err != nil {
			return fmt.Errorf("prepare assume-existing Ninja: %w", err)
		}
		fmt.Printf("uni: assume existing unlogged outputs; rebuild API validation outputs\n")
	}
	report.event("recovery trust_output=%t assume_existing=%t", options.TrustOutput, options.AssumeExisting)
	if err := prepareNinjaState(outDir, options.TrustOutput); err != nil {
		return fmt.Errorf("prepare Ninja recovery state: %w", err)
	}
	report.event("resources cgroup=%t ccache=%t compiler_check_auto=%t depend_mode_auto=%t fileclone_auto=%t ccache_max_size_auto=%q",
		runner.useCgroup, runner.useCcache,
		runner.autoCcacheCompilerCheck, runner.autoCcacheDepend,
		runner.autoCcacheFileClone, runner.autoCcacheMaxSize)
	buildExecutor := executorLabel(runner.requestedNinja)
	if runner.forceLocalNinja {
		buildExecutor = runner.phasedNinja
	}
	report.event("executor requested=%s build=%s auto_local=%t",
		executorLabel(runner.requestedNinja), buildExecutor, autoLocalNinja)
	jobs := MaximumJobs(options.MaxJobs)

	release := os.Getenv("TARGET_RELEASE")
	variant := os.Getenv("TARGET_BUILD_VARIANT")
	var state State
	var reused bool
	if options.ForceReuse {
		state, reused, err = ForceReuseState(statePath, top, outDir, product, release, variant, options)
	} else {
		state, reused, err = ReuseState(statePath, top, outDir, product, release, variant, options)
	}
	if err != nil {
		return fmt.Errorf("reuse prepared graph: %w", err)
	}
	graphArgs := prepareArgs(options, jobs)
	if reused {
		fmt.Printf("uni: reuse graph\n")
		report.event("graph reused=true")
	} else {
		fmt.Printf("uni: prepare graph, -j%d\n", jobs)
		if _, err := runner.runReported(ctx, report, &summary, "graph-analysis", "--uni-prepare-mode", "prepare", statePath, graphArgs, jobs); err != nil {
			return err
		}
		state, err = LoadState(statePath)
		if err != nil {
			return fmt.Errorf("load prepared graph: %w", err)
		}
		state, err = RecordSourceFingerprint(statePath, top, outDir, state)
		if err != nil {
			return fmt.Errorf("record source graph: %w", err)
		}
		if err := state.Validate(top, outDir, product); err != nil {
			return fmt.Errorf("invalid prepared graph: %w", err)
		}
	}
	if !options.FullBuild {
		if options.Plan {
			printSinglePhasePlan(state, jobs)
			return nil
		}
		fmt.Printf("uni: one Ninja phase, %d target(s), -j%d\n", len(state.NinjaArgs), jobs)
		_, err := runner.runReported(ctx, report, &summary, "ninja", "--uni-ninja-mode", "only", statePath,
			phaseArgs(options, state.NinjaArgs, jobs, state.Dist), jobs)
		if err == nil {
			report.event("build result=success phases=%d min_mem_available=%s swap_out=%s",
				summary.phases, formatBytes(summary.minimumAvailable), formatBytes(int64(summary.swapOutBytes)))
			printBuildComplete(started, summary, state)
		}
		return err
	}

	// For full builds, graph preparation is the only build phase managed by
	// uni. Once the graph is ready, hand the complete target set to one
	// unmanaged executor invocation. This keeps uni's analysis memory guard while
	// avoiding package batching, R8 scheduling, kernel insertion, pool
	// overrides, and memory-pressure process restarts during the actual build.
	if options.Dev || options.DevAuto {
		r8Started := time.Now()
		r8Modules, r8Source, r8Err := LoadR8ModulesForModeContext(ctx, state,
			filepath.Join(stateDir, "r8_modules.json"), r8Mode)
		report.analysis("r8", r8Source, len(r8Modules), r8Started, r8Err, outDir)
		if r8Err != nil {
			return fmt.Errorf("index R8 modules: %w", r8Err)
		}
	}
	if options.Plan {
		printSinglePhasePlan(state, jobs)
		return nil
	}

	targets := finalNinjaTargets(state, options, "")
	fmt.Printf("uni: hand off to build executor, %d target(s), -j%d\n", len(targets), jobs)
	_, err = runner.runUnmanagedReported(ctx, report, &summary, "build", "--uni-ninja-mode", "only", statePath,
		phaseArgs(options, targets, jobs, state.Dist), jobs)
	if err == nil {
		if options.SignKeys != "" {
			result, signingErr := runSigning(ctx, top, outDir, state, options)
			if signingErr != nil {
				return fmt.Errorf("sign build: %w", signingErr)
			}
			report.event("signing check=false source=%s target_files=%s ota=%s checksum=%s", result.SourceTargetFiles, result.SignedTargetFiles, result.SignedOTA, result.Checksum)
			fmt.Printf("uni: signed package=%s\n", result.SignedOTA)
			fmt.Printf("uni: checksum=%s\n", result.Checksum)
		}
		report.event("build result=success phases=%d min_mem_available=%s swap_out=%s",
			summary.phases, formatBytes(summary.minimumAvailable), formatBytes(int64(summary.swapOutBytes)))
		printBuildComplete(started, summary, state)
	}
	return err
}
