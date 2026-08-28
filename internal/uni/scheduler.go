// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	startupCleanup := terminateResidualBuildProcesses(outDir)
	report.event("process_cleanup when=start found=%d term_sent=%d kill_sent=%d remaining=%d",
		startupCleanup.Found, startupCleanup.TermSent, startupCleanup.KillSent, startupCleanup.Remaining)
	if startupCleanup.Remaining > 0 {
		return fmt.Errorf("%d residual uni build process(es) survived cleanup", startupCleanup.Remaining)
	}
	report.event("options jobs=%d load_set=%t load=%.2f batch=%d static=%t plan=%t debug=%t dev=%t dev_auto=%t trust_output=%t assume_existing=%t force_reuse=%t full_build=%t dist=%t targets=%d key_values=%d",
		options.MaxJobs, options.LoadSet, options.LoadAverage, options.BatchSize, options.Static, options.Plan,
		options.Debug, options.Dev, options.DevAuto, options.TrustOutput, options.AssumeExisting,
		options.ForceReuse, options.FullBuild, options.Dist, len(options.Targets), len(options.KeyValues))
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
	runner.forceLocalNinja = options.FullBuild
	if options.FullBuild {
		runner.kernelJobs = nestedKernelJobs(options.MaxJobs)
	}
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
		fmt.Printf("uni: assume existing outputs with missing Ninja log entries\n")
	}
	report.event("recovery trust_output=%t assume_existing=%t", options.TrustOutput, options.AssumeExisting)
	if err := prepareNinjaState(outDir, options.TrustOutput); err != nil {
		return fmt.Errorf("prepare Ninja recovery state: %w", err)
	}
	report.event("resources cgroup=%t ccache=%t compiler_check_auto=%t fileclone_auto=%t",
		runner.useCgroup, runner.useCcache,
		runner.autoCcacheCompilerCheck, runner.autoCcacheFileClone)
	singleExecutor := executorLabel(runner.requestedNinja)
	if runner.forceLocalNinja {
		singleExecutor = runner.phasedNinja
	}
	report.event("executor requested=%s single=%s segmented=%s",
		executorLabel(runner.requestedNinja), singleExecutor, runner.phasedNinja)
	if runner.kernelJobs > 0 {
		report.event("kernel nested_jobs=%d global_jobs=%d", runner.kernelJobs, MaximumJobs(options.MaxJobs))
	}
	snapshot := memorySnapshotOrWarning(report, "initial-memory")
	jobs := MaximumJobs(options.MaxJobs)
	batchSize := options.BatchSize

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
		runner.disableIncrementalAnalysis()
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

	packages := StableShuffle(ProductTargets(state), state.TargetProduct)
	longWeights, historySource := historyWeights(top, state, runner)
	packages = InterleaveLongTargets(packages, longWeights)
	report.event("history schedule=%q", historySource)
	snapshot = memorySnapshotOrWarning(report, "schedule-memory")
	batchSize = scheduledBatchSize(options, len(packages), snapshot)
	singleGraph := useSingleGraph(packages, batchSize)
	r8Started := time.Now()
	r8Modules, r8Source, err := LoadR8ModulesForModeContext(ctx, state, filepath.Join(stateDir, "r8_modules.json"), r8Mode)
	report.analysis("r8", r8Source, len(r8Modules), r8Started, err, outDir)
	if err != nil {
		return fmt.Errorf("inspect R8 modules: %w", err)
	}
	packages = InterleaveR8Targets(packages, r8Modules)
	r8PerBatch := scheduledR8Limit(packages, r8Modules, batchSize)
	earlyKernel, kernelPriority, err := EarlyKernelTargets(state.KatiBuildNinja)
	if err != nil {
		return fmt.Errorf("inspect kernel target: %w", err)
	}
	startup := selectStartupSchedule(packages, longWeights, r8Modules, earlyKernel, jobs)
	// A single full graph already lets Ninja schedule all package work globally.
	// Priming R8/Kotlin beside the nested kernel build caused heavy swap without
	// shortening the final graph, so keep the proven exclusive-kernel fast path.
	startup = constrainStartupForGraph(startup, earlyKernel, singleGraph)
	startupJobs := startupPhaseJobs(jobs, runner.kernelJobs, earlyKernel != "")
	runner.sisoPriorityTargets = nil
	if kernelPriority != "" {
		runner.sisoPriorityTargets = []string{kernelPriority}
	}
	report.event("schedule packages=%d r8=%d r8_per_batch=%d batch=%d jobs=%d early_kernel=%s kernel_priority=%s single_graph=%t startup_jobs=%d startup_packages=%d startup_history=%d startup_r8=%d startup_non_r8=%d startup_targets=%q",
		len(packages), R8TargetCount(packages, r8Modules), r8PerBatch, batchSize, jobs, earlyKernel, kernelPriority, singleGraph,
		startupJobs, startup.packageCount, startup.historyCount, startup.r8Count, startup.nonR8Count, startup.targets)
	for _, target := range startup.targets {
		if duration := longWeights[target]; duration > 0 {
			_, isR8 := r8Modules[target]
			report.event("startup_history target=%q duration_ms=%.0f r8=%t", target, duration, isR8)
		}
	}
	if options.Plan {
		printPlan(state, packages, startup, R8TargetCount(packages, r8Modules), batchSize, startupJobs, jobs, earlyKernel)
		return nil
	}

	completed := make(map[string]struct{}, len(packages))
	ninjaStarted := false
	finalRan := false
	refreshes := 0
	segmentNumber := 0
	startupPending := len(startup.targets) > 0
	for {
		if err := state.Validate(top, outDir, product); err != nil {
			if refreshes >= 1 {
				return fmt.Errorf("build graph changed repeatedly: %w", err)
			}
			fmt.Printf("uni: graph changed; prepare again\n")
			if _, runErr := runner.runReported(ctx, report, &summary, "graph-analysis-refresh", "--uni-prepare-mode", "prepare", statePath, graphArgs, jobs); runErr != nil {
				return runErr
			}
			state, err = LoadState(statePath)
			if err != nil {
				return err
			}
			packages = StableShuffle(ProductTargets(state), state.TargetProduct)
			longWeights, historySource = historyWeights(top, state, runner)
			packages = InterleaveLongTargets(packages, longWeights)
			report.event("history_refresh schedule=%q", historySource)
			earlyKernel, kernelPriority, err = EarlyKernelTargets(state.KatiBuildNinja)
			if err != nil {
				return err
			}
			runner.sisoPriorityTargets = nil
			if kernelPriority != "" {
				runner.sisoPriorityTargets = []string{kernelPriority}
			}
			r8Started = time.Now()
			r8Modules, r8Source, err = LoadR8ModulesForModeContext(ctx, state, filepath.Join(stateDir, "r8_modules.json"), r8Mode)
			report.analysis("r8-refresh", r8Source, len(r8Modules), r8Started, err, outDir)
			if err != nil {
				return err
			}
			packages = InterleaveR8Targets(packages, r8Modules)
			snapshot = memorySnapshotOrWarning(report, "refresh-memory")
			batchSize = scheduledBatchSize(options, len(packages), snapshot)
			singleGraph = useSingleGraph(packages, batchSize)
			r8PerBatch = scheduledR8Limit(packages, r8Modules, batchSize)
			startup = selectStartupSchedule(packages, longWeights, r8Modules, earlyKernel, jobs)
			completed = make(map[string]struct{}, len(packages))
			startupPending = len(startup.targets) > 0
			finalRan = false
			refreshes++
			continue
		}
		if startupPending {
			phase := "first"
			name := "startup"
			phaseJobs := startupPhaseJobs(jobs, runner.kernelJobs, earlyKernel != "")
			kernelOnly := earlyKernel != "" && startup.packageCount == 0 && len(startup.targets) == 1
			if kernelOnly {
				name = "kernel"
				phaseJobs = jobs
			}
			if ninjaStarted {
				phase = "middle"
				if kernelOnly {
					name = "kernel-refresh"
				} else {
					name = "startup-refresh"
				}
			}
			if kernelOnly {
				fmt.Printf("uni: exclusive kernel phase (%s), -j%d\n", earlyKernel, phaseJobs)
			} else {
				fmt.Printf("uni: startup phase, %d target(s), history=%d R8=%d non-R8=%d, -j%d\n",
					len(startup.targets), startup.historyCount, startup.r8Count, startup.nonR8Count, phaseJobs)
			}
			if _, runErr := runner.runReported(ctx, report, &summary, name, "--uni-ninja-mode", phase, statePath,
				phaseArgs(options, startup.targets, phaseJobs, false), phaseJobs); runErr != nil {
				return runErr
			}
			ninjaStarted = true
			startupPending = false
			for _, target := range startup.targets {
				if target != earlyKernel {
					completed[target] = struct{}{}
				}
			}
		}
		if singleGraph {
			break
		}

		remaining := removeCompleted(append([]string(nil), packages...), completed)
		if len(remaining) == 0 {
			break
		}
		r8PerBatch = scheduledR8Limit(remaining, r8Modules, batchSize)
		batch := takeBatchWithR8Limit(remaining, batchSize, r8Modules, r8PerBatch)
		count := len(batch)
		phase := "middle"
		if !ninjaStarted {
			phase = "first"
		}
		segmentNumber++
		remainingAfter := len(remaining) - count
		name := fmt.Sprintf("segment-%d", segmentNumber)
		targets := append([]string(nil), batch...)
		dist := false
		if !ninjaStarted && earlyKernel != "" {
			targets = append(targets, earlyKernel)
		}
		if remainingAfter == 0 {
			name = "final"
			phase = "final"
			finalTargets := finalNinjaTargets(state, options, "")
			targets = append(targets, finalTargets...)
			dist = state.Dist
			fmt.Printf("uni: final segment, %d package target(s), -j%d\n", len(batch), jobs)
		} else {
			fmt.Printf("uni: segment %d, %d target(s), %d remaining, -j%d\n",
				segmentNumber, len(batch), remainingAfter, jobs)
		}
		_, err := runner.runReported(ctx, report, &summary, name, "--uni-ninja-mode", phase, statePath,
			phaseArgs(options, targets, jobs, dist), jobs)
		if err != nil {
			return err
		}
		ninjaStarted = true
		finalRan = remainingAfter == 0
		for _, target := range batch {
			completed[target] = struct{}{}
		}
		if finalRan {
			break
		}
	}

	if !finalRan {
		if err := state.Validate(top, outDir, product); err != nil {
			return fmt.Errorf("build graph changed before final phase: %w", err)
		}
		finalKernel := earlyKernel
		if ninjaStarted {
			finalKernel = ""
		}
		finalTargets := finalNinjaTargets(state, options, finalKernel)
		finalPhase := "final"
		if !ninjaStarted {
			finalPhase = "only"
		}
		fmt.Printf("uni: final phase, %d target(s), -j%d\n", len(finalTargets), jobs)
		_, err = runner.runReported(ctx, report, &summary, "final", "--uni-ninja-mode", finalPhase, statePath,
			phaseArgs(options, finalTargets, jobs, state.Dist), jobs)
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
	}
	if err == nil {
		if state.TaskMetadata == "" {
			report.event("history_write result=skipped reason=task-metadata-disabled")
		} else {
			if historyErr := recordBuildHistory(top, state, runner, summary); historyErr != nil {
				fmt.Fprintf(os.Stderr, "uni: history warning: %v\n", historyErr)
				report.event("history_write error=%q", historyErr)
			} else {
				report.event("history_write result=ok path=%s", historyPath(top, state.TargetProduct))
			}
		}
		report.event("build result=success phases=%d min_mem_available=%s swap_out=%s",
			summary.phases, formatBytes(summary.minimumAvailable), formatBytes(int64(summary.swapOutBytes)))
		printBuildComplete(started, summary, state)
	}
	return err
}
