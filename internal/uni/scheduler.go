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

func printPlan(state State, packages, r8Prime []string, r8Count, batchSize, jobs int, earlyKernel string) {
	segments := len(Batches(packages, batchSize))
	fmt.Printf("product: %s (%s-%s)\n", state.TargetProduct, state.TargetRelease, state.BuildVariant)
	fmt.Printf("graph: %s\n", state.CombinedNinja)
	fmt.Printf("packages: %d\n", len(packages))
	fmt.Printf("R8 modules: %d, prime: %d\n", r8Count, len(r8Prime))
	if segments <= 1 {
		fmt.Printf("segments: 1 final graph\n")
	} else if len(r8Prime) > 0 {
		fmt.Printf("segments: 1 R8 prime + %d package + 1 final\n", segments)
	} else {
		fmt.Printf("segments: %d package + 1 final\n", segments)
	}
	fmt.Printf("initial: %d targets, -j%d\n", batchSize, jobs)
	if earlyKernel == "" {
		fmt.Printf("kernel exclusive first: false\n")
	} else {
		fmt.Printf("kernel exclusive first: %s\n", earlyKernel)
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

func scheduledR8PrimeTargets(packages []string, r8Modules map[string]struct{}, jobs, batchSize int) []string {
	segments := len(Batches(packages, batchSize))
	if segments <= 1 {
		return nil
	}
	return R8PrimeTargets(packages, r8Modules, jobs, segments)
}

func useSingleGraph(packages []string, batchSize int) bool {
	return len(Batches(packages, batchSize)) <= 1
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
		report, err = newDebugReport(outDir, product, options.BuildArgs)
		if err != nil {
			return fmt.Errorf("create debug report: %w", err)
		}
		fmt.Printf("uni: debug report: %s\n", report.path)
		report.event("paths source=%s out=%s", top, outDir)
		defer report.close(outDir)
	}
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
	report.event("optimization rust_incremental=%t partial_compile=%t kotlin_incremental_client=%t kotlin_daemon=%t critical_path=%s soong_incremental_analysis=%t",
		runner.rustIncremental, runner.partialCompile, runner.partialCompile, runner.kotlinDaemon,
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
	report.event("executor requested=%s phased=%s",
		executorLabel(runner.requestedNinja), runner.phasedNinja)
	snapshot, err := ReadMemorySnapshot()
	if err != nil {
		return err
	}
	jobs := InitialJobs(options.MaxJobs, snapshot)
	if options.Static {
		jobs = MaximumJobs(options.MaxJobs)
	}
	batchSize := options.BatchSize

	fmt.Printf("uni: prepare graph, -j%d\n", jobs)
	graphArgs := prepareArgs(options, jobs)
	if _, err := runner.runReported(ctx, report, &summary, "graph-analysis", "--uni-prepare-mode", "prepare", statePath, graphArgs, jobs); err != nil {
		return err
	}
	state, err := LoadState(statePath)
	if err != nil {
		return fmt.Errorf("load prepared graph: %w", err)
	}
	if err := state.Validate(top, outDir, product); err != nil {
		return fmt.Errorf("invalid prepared graph: %w", err)
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
	snapshot, err = ReadMemorySnapshot()
	if err != nil {
		return err
	}
	batchSize = scheduledBatchSize(options, len(packages), snapshot)
	singleGraph := useSingleGraph(packages, batchSize)
	r8Started := time.Now()
	r8Modules, r8Source, err := LoadR8ModulesForModeContext(ctx, state, filepath.Join(stateDir, "r8_modules.json"), r8Mode)
	report.analysis("r8", r8Source, len(r8Modules), r8Started, err, outDir)
	if err != nil {
		return fmt.Errorf("inspect R8 modules: %w", err)
	}
	packages = InterleaveR8Targets(packages, r8Modules)
	r8Prime := scheduledR8PrimeTargets(packages, r8Modules, HighmemJobs(jobs, snapshot), batchSize)
	earlyKernel, err := EarlyKernelTarget(state.KatiBuildNinja)
	if err != nil {
		return fmt.Errorf("inspect kernel target: %w", err)
	}
	report.event("schedule packages=%d r8=%d prime=%d batch=%d jobs=%d early_kernel=%s single_graph=%t",
		len(packages), R8TargetCount(packages, r8Modules), len(r8Prime), batchSize, jobs, earlyKernel, singleGraph)
	if options.Plan {
		printPlan(state, packages, r8Prime, R8TargetCount(packages, r8Modules), batchSize, jobs, earlyKernel)
		return nil
	}

	completed := make(map[string]struct{}, len(packages))
	kernelCompleted := false
	r8Primed := false
	ninjaStarted := false
	refreshes := 0
	segmentNumber := 0
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
			earlyKernel, err = EarlyKernelTarget(state.KatiBuildNinja)
			if err != nil {
				return err
			}
			r8Started = time.Now()
			r8Modules, r8Source, err = LoadR8ModulesForModeContext(ctx, state, filepath.Join(stateDir, "r8_modules.json"), r8Mode)
			report.analysis("r8-refresh", r8Source, len(r8Modules), r8Started, err, outDir)
			if err != nil {
				return err
			}
			packages = InterleaveR8Targets(packages, r8Modules)
			snapshot, err = ReadMemorySnapshot()
			if err != nil {
				return err
			}
			batchSize = scheduledBatchSize(options, len(packages), snapshot)
			singleGraph = useSingleGraph(packages, batchSize)
			r8Prime = scheduledR8PrimeTargets(packages, r8Modules, HighmemJobs(jobs, snapshot), batchSize)
			kernelCompleted = false
			r8Primed = false
			completed = make(map[string]struct{}, len(packages))
			refreshes++
			continue
		}
		if earlyKernel != "" && !kernelCompleted {
			kernelJobs := MaximumJobs(options.MaxJobs)
			phase := "middle"
			if !ninjaStarted {
				phase = "first"
			}
			fmt.Printf("uni: exclusive kernel phase (%s), -j%d\n", earlyKernel, kernelJobs)
			if _, err := runner.runReported(ctx, report, &summary, "kernel", "--uni-ninja-mode", phase, statePath,
				phaseArgs(options, []string{earlyKernel}, kernelJobs, false),
				kernelJobs); err != nil {
				return err
			}
			kernelCompleted = true
			ninjaStarted = true
			jobs = MaximumJobs(options.MaxJobs)
			snapshot, err = ReadMemorySnapshot()
			if err != nil {
				return err
			}
			batchSize = scheduledBatchSize(options, len(packages), snapshot)
			singleGraph = useSingleGraph(packages, batchSize)
			r8Prime = scheduledR8PrimeTargets(packages, r8Modules, HighmemJobs(jobs, snapshot), batchSize)
			fmt.Printf("uni: kernel complete; restore -j%d, batch %d\n", jobs, batchSize)
			continue
		}
		if singleGraph {
			break
		}
		if !r8Primed {
			r8Primed = true
			prime := removeCompleted(append([]string(nil), r8Prime...), completed)
			if len(prime) == 0 {
				continue
			}
			phase := "middle"
			if !ninjaStarted {
				phase = "first"
			}
			fmt.Printf("uni: R8 prime, %d module(s), -j%d\n", len(prime), jobs)
			sample, err := runner.runReported(ctx, report, &summary, "r8-prime", "--uni-ninja-mode", phase, statePath,
				phaseArgs(options, prime, jobs, false), jobs)
			if err != nil {
				return err
			}
			ninjaStarted = true
			for _, target := range prime {
				completed[target] = struct{}{}
			}
			if !options.Static {
				previousJobs := jobs
				_, jobs = Adapt(batchSize, jobs, options.MaxJobs, sample)
				report.event("adapt phase=r8-prime jobs=%d->%d", previousJobs, jobs)
			}
			continue
		}

		remaining := removeCompleted(append([]string(nil), packages...), completed)
		if len(remaining) == 0 {
			break
		}
		count := min(batchSize, len(remaining))
		batch := append([]string(nil), remaining[:count]...)
		phase := "middle"
		if !ninjaStarted {
			phase = "first"
		}
		segmentNumber++
		remainingAfter := len(remaining) - count
		fmt.Printf("uni: segment %d, %d target(s), %d remaining, -j%d\n",
			segmentNumber, len(batch), remainingAfter, jobs)
		sample, err := runner.runReported(ctx, report, &summary, fmt.Sprintf("segment-%d", segmentNumber), "--uni-ninja-mode", phase, statePath,
			phaseArgs(options, batch, jobs, false), jobs)
		if err != nil {
			return err
		}
		ninjaStarted = true
		for _, target := range remaining[:count] {
			completed[target] = struct{}{}
		}
		if !options.Static {
			previousBatch, previousJobs := batchSize, jobs
			batchSize, jobs = Adapt(batchSize, jobs, options.MaxJobs, sample)
			report.event("adapt phase=segment-%d batch=%d->%d jobs=%d->%d",
				segmentNumber, previousBatch, batchSize, previousJobs, jobs)
		}
	}

	if err := state.Validate(top, outDir, product); err != nil {
		return fmt.Errorf("build graph changed before final phase: %w", err)
	}
	finalTargets := append([]string(nil), state.NinjaArgs...)
	if len(finalTargets) == 0 && len(options.Targets) > 0 {
		finalTargets = append(finalTargets, options.Targets...)
	}
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
	if err == nil {
		report.event("build result=success phases=%d min_mem_available=%s swap_out=%s",
			summary.phases, formatBytes(summary.minimumAvailable), formatBytes(int64(summary.swapOutBytes)))
		printBuildComplete(started, summary, state)
	}
	return err
}
