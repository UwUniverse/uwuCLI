// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

var errMemoryPressure = errors.New("sustained memory pressure: build stopped and recovery state saved")

const (
	memoryRecoveryPSI           = 10.0
	memoryRecoverySamples       = 3
	memoryRecoveryPollInterval  = time.Second
	memoryRecoveryTimeout       = 45 * time.Second
	runaParallelismRampInterval = 15 * time.Second
	runaParallelismRampMaxPSI   = 10.0
	runaSwapInPressureRate      = 4 * 1024 * 1024  // 240 MiB/min
	runaSwapOutPressureRate     = 16 * 1024 * 1024 // 960 MiB/min
)

type swapRates struct {
	inBytesPerSecond  float64
	outBytesPerSecond float64
}

func (rates swapRates) active() bool {
	return rates.inBytesPerSecond >= runaSwapInPressureRate || rates.outBytesPerSecond >= runaSwapOutPressureRate
}

type swapRateSampler struct {
	previousAt  time.Time
	previousIn  uint64
	previousOut uint64
}

func (sampler *swapRateSampler) observe(now time.Time, memory MemorySnapshot) swapRates {
	if sampler.previousAt.IsZero() || !now.After(sampler.previousAt) ||
		memory.SwapInPage < sampler.previousIn || memory.SwapOutPage < sampler.previousOut {
		sampler.previousAt = now
		sampler.previousIn = memory.SwapInPage
		sampler.previousOut = memory.SwapOutPage
		return swapRates{}
	}
	seconds := now.Sub(sampler.previousAt).Seconds()
	pageSize := float64(os.Getpagesize())
	rates := swapRates{
		inBytesPerSecond:  float64(memory.SwapInPage-sampler.previousIn) * pageSize / seconds,
		outBytesPerSecond: float64(memory.SwapOutPage-sampler.previousOut) * pageSize / seconds,
	}
	sampler.previousAt = now
	sampler.previousIn = memory.SwapInPage
	sampler.previousOut = memory.SwapOutPage
	return rates
}

type runaParallelismRamp struct {
	healthySince time.Time
	lastIncrease time.Time
}

func runaParallelismCeiling(inferredJobs int) int {
	if inferredJobs < 1 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if inferredJobs > maxInt/5 {
		return maxInt
	}
	return inferredJobs * 5 / 2
}

func (ramp *runaParallelismRamp) observe(now time.Time, memory MemorySnapshot, fullAvg10 float64, swap swapRates, currentJobs, inferredJobs int) (int, int, bool) {
	ceiling := runaParallelismCeiling(inferredJobs)
	minimumAvailable := max(6*gibibyte, memory.Total/5)
	if memory.Total <= 0 || memory.Available < minimumAvailable || fullAvg10 >= runaParallelismRampMaxPSI || swap.active() ||
		currentJobs < 1 || inferredJobs < 1 || currentJobs >= ceiling {
		ramp.healthySince = time.Time{}
		return currentJobs, ceiling, false
	}
	if ramp.healthySince.IsZero() {
		ramp.healthySince = now
		return currentJobs, ceiling, false
	}
	if now.Sub(ramp.healthySince) < runaParallelismRampInterval ||
		(!ramp.lastIncrease.IsZero() && now.Sub(ramp.lastIncrease) < runaParallelismRampInterval) {
		return currentJobs, ceiling, false
	}
	step := max(1, inferredJobs/6)
	nextJobs := ceiling
	if step < ceiling-currentJobs {
		nextJobs = currentJobs + step
	}
	if nextJobs <= currentJobs {
		return currentJobs, ceiling, false
	}
	ramp.lastIncrease = now
	return nextJobs, ceiling, true
}

func (ramp *runaParallelismRamp) reset() {
	ramp.healthySince = time.Time{}
}

func memoryRetryJobs(err, contextErr error, jobs int) (int, bool) {
	if !errors.Is(err, errMemoryPressure) || contextErr != nil || jobs < 1 {
		return jobs, false
	}
	return max(1, jobs*2/3), true
}

type memoryPressureGuard struct {
	since time.Time
}

func (guard *memoryPressureGuard) observe(now time.Time, memory MemorySnapshot, fullAvg10 float64, swap swapRates, linkerHeavy bool) bool {
	if memory.Total <= 0 || memory.Available < 0 {
		guard.since = time.Time{}
		return false
	}
	reserve := max(3*gibibyte, memory.Total/8)
	pressured := memory.Available < reserve && fullAvg10 >= 20
	if memory.Available < memory.Total/3 && fullAvg10 >= runaParallelismRampMaxPSI && swap.active() {
		pressured = true
	}
	if memory.Available < memory.Total/3 && fullAvg10 >= 45 {
		pressured = true
	}
	if linkerHeavy && memory.Available < 2*reserve && fullAvg10 >= 35 {
		pressured = true
	}
	if memory.Available >= 0 && memory.Available < gibibyte/2 {
		pressured = true
	}
	if !pressured {
		guard.since = time.Time{}
		return false
	}
	if guard.since.IsZero() {
		guard.since = now
	}
	return now.Sub(guard.since) >= 6*time.Second
}

type memoryRecoveryGuard struct {
	healthy int
}

func (guard *memoryRecoveryGuard) observe(memory MemorySnapshot, fullAvg10 float64) bool {
	if memory.Total <= 0 || memory.Available < 0 {
		guard.healthy = 0
		return false
	}
	reserve := max(4*gibibyte, memory.Total/8)
	if memory.Available < reserve || fullAvg10 > memoryRecoveryPSI {
		guard.healthy = 0
		return false
	}
	guard.healthy++
	return guard.healthy >= memoryRecoverySamples
}

func waitForMemoryRecovery(ctx context.Context, timeout time.Duration) (time.Duration, bool) {
	started := time.Now()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(memoryRecoveryPollInterval)
	defer ticker.Stop()
	var guard memoryRecoveryGuard
	for {
		memory, memoryErr := ReadMemorySnapshot()
		psi, psiErr := readMemoryPSI()
		if memoryErr == nil && psiErr == nil && guard.observe(memory, psi.full.avg10) {
			return time.Since(started), true
		}
		select {
		case <-ctx.Done():
			return time.Since(started), false
		case <-deadline.C:
			return time.Since(started), false
		case <-ticker.C:
		}
	}
}

func replaceParallelArgs(args []string, jobs int) []string {
	result := []string{"-j" + strconv.Itoa(jobs)}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(result, args[i:]...)
		}
		if arg == "-j" && i+1 < len(args) {
			if _, err := strconv.Atoi(args[i+1]); err == nil {
				i++
				continue
			}
		}
		if strings.HasPrefix(arg, "-j") {
			if _, err := strconv.Atoi(strings.TrimPrefix(arg, "-j")); err == nil {
				continue
			}
		}
		result = append(result, arg)
	}
	return result
}
