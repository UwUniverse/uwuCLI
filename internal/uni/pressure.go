// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

var errMemoryPressure = errors.New("sustained memory pressure: build stopped and recovery state saved")

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
