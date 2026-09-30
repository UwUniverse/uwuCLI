// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"fmt"
	"time"
)

type parallelismControl interface {
	setParallelism(context.Context, int) (string, error)
}

// Changes admission only. It has no process handles and cannot signal or cancel a task.
type adaptiveParallelism struct {
	jobs, ceiling int
	pressure      memoryPressureGuard
	ramp          runaParallelismRamp
	nextChange    time.Time
}

type parallelismChange struct {
	previous, jobs   int
	reason, response string
	err              error
}

func (state *adaptiveParallelism) observe(ctx context.Context, now time.Time,
	memory MemorySnapshot, fullAvg10 float64, swap swapRates, linkerHeavy bool,
	control parallelismControl) parallelismChange {
	change := parallelismChange{previous: state.jobs, jobs: state.jobs}
	pressured := state.pressure.observe(now, memory, fullAvg10, swap, linkerHeavy)
	if pressured {
		state.ramp.reset()
		if now.Before(state.nextChange) || state.jobs <= 1 {
			return change
		}
		state.nextChange = now.Add(20 * time.Second)
		if control == nil {
			change.reason = "pressure"
			change.err = fmt.Errorf("executor has no runtime admission control; keeping running tasks")
			return change
		}
		change.reason = "pressure"
		change.jobs = max(1, state.jobs-state.jobs/4)
		if change.jobs == state.jobs {
			change.jobs--
		}
	} else {
		if control == nil || now.Before(state.nextChange) {
			return change
		}
		next, _, increase := state.ramp.observe(now, memory, fullAvg10, swap, state.jobs, state.ceiling)
		if !increase {
			return change
		}
		change.jobs, change.reason = next, "recovery"
	}
	response, err := control.setParallelism(ctx, change.jobs)
	if err != nil {
		// No assumed acknowledgement: retain the last confirmed limit and retry later.
		state.nextChange = now.Add(10 * time.Second)
		state.ramp.reset()
		change.jobs, change.err = state.jobs, err
		return change
	}
	state.jobs = change.jobs
	change.response = response
	return change
}
