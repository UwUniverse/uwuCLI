// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

var errMemoryPressure = errors.New("sustained memory pressure: build stopped and recovery state saved")

const (
	memoryEmergencyPSI         = 45.0
	memoryRecoveryPSI          = 10.0
	memoryRecoverySamples      = 3
	memoryRecoveryPollInterval = time.Second
	memoryRecoveryTimeout      = 45 * time.Second
)

func memoryRetryJobs(err, contextErr error, jobs, retries int) (int, bool) {
	if !errors.Is(err, errMemoryPressure) || contextErr != nil || jobs <= 1 || retries >= 2 {
		return jobs, false
	}
	return max(1, jobs*2/3), true
}

type memoryPressureGuard struct {
	since time.Time
}

func (guard *memoryPressureGuard) observe(now time.Time, memory MemorySnapshot, fullAvg10 float64) bool {
	if memory.Total <= 0 || memory.Available < 0 {
		guard.since = time.Time{}
		return false
	}
	reserve := max(3*gibibyte, memory.Total/8)
	pressured := memory.Available < reserve && fullAvg10 >= 20
	if fullAvg10 >= memoryEmergencyPSI {
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
