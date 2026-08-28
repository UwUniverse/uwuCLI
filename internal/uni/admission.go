// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"sort"
	"strings"
)

type poolDecision struct {
	highmem         int
	r8              int
	rust            int
	java            int
	kotlin          int
	highmemExplicit bool
	r8Explicit      bool
	rustExplicit    bool
	javaExplicit    bool
	kotlinExplicit  bool
	reason          string
}

type poolAdmission struct{}

func (admission *poolAdmission) decide(maxJobs int, snapshot MemorySnapshot, environment []string, rustCodegenUnits int) poolDecision {
	limit := MaximumJobs(maxJobs)
	decision := poolDecision{
		highmem: HighmemJobs(limit, snapshot),
		r8:      R8Jobs(limit, snapshot),
		rust:    RustJobs(limit, snapshot, rustCodegenUnits),
		java:    JavaJobs(limit, snapshot),
		kotlin:  KotlinJobs(limit, snapshot),
		reason:  "memory-throughput-balance",
	}
	if value, valid := positiveEnvironmentInt(environment, "NINJA_HIGHMEM_NUM_JOBS"); valid {
		decision.highmem = value
		decision.highmemExplicit = true
	}
	if value, valid := positiveEnvironmentInt(environment, "NINJA_UNI_R8_NUM_JOBS"); valid {
		decision.r8 = value
		decision.r8Explicit = true
	}
	if value, valid := positiveEnvironmentInt(environment, "NINJA_UNI_RUST_NUM_JOBS"); valid {
		decision.rust = value
		decision.rustExplicit = true
	}
	if value, valid := positiveEnvironmentInt(environment, "NINJA_UNI_JAVA_NUM_JOBS"); valid {
		decision.java = value
		decision.javaExplicit = true
	}
	if value, valid := positiveEnvironmentInt(environment, "NINJA_UNI_KOTLIN_NUM_JOBS"); valid {
		decision.kotlin = value
		decision.kotlinExplicit = true
	}
	var explicit []string
	for name, set := range map[string]bool{
		"highmem": decision.highmemExplicit,
		"r8":      decision.r8Explicit,
		"rust":    decision.rustExplicit,
		"java":    decision.javaExplicit,
		"kotlin":  decision.kotlinExplicit,
	} {
		if set {
			explicit = append(explicit, name)
		}
	}
	if len(explicit) > 0 {
		sort.Strings(explicit)
		decision.reason += "; explicit=" + strings.Join(explicit, ",")
	}
	return decision
}

func (admission *poolAdmission) observe(_ SegmentSample) {
	// Every phase takes a fresh MemAvailable snapshot. Pool depth is recomputed
	// at the next phase boundary without reducing global Ninja concurrency.
}
