// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import "testing"

func TestRustPoolMatchesSoongCodegenDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want int
	}{
		{"userdebug", []string{"TARGET_BUILD_VARIANT=userdebug"}, 1},
		{"user", []string{"TARGET_BUILD_VARIANT=user"}, 1},
		{"eng", []string{"TARGET_BUILD_VARIANT=eng"}, 16},
		{"incremental", []string{"TARGET_BUILD_VARIANT=eng", "SOONG_RUSTC_INCREMENTAL=true"}, 256},
		{"explicit", []string{"SOONG_RUSTC_CODEGEN_UNITS=4"}, 4},
		{"invalid", []string{"SOONG_RUSTC_CODEGEN_UNITS=invalid"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rustCodegenUnitsForPool(tc.env); got != tc.want {
				t.Fatalf("codegen units = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRustPoolDoesNotReserveUnusedCodegenWorkers(t *testing.T) {
	snapshot := MemorySnapshot{Total: 32577765376, Available: 27971477504}
	var admission poolAdmission
	old := admission.decide(18, snapshot, nil, 4)
	got := admission.decide(18, snapshot, nil, rustCodegenUnitsForPool([]string{"TARGET_BUILD_VARIANT=userdebug"}))
	if old.rust != 5 || got.rust != 11 {
		t.Fatalf("Rust slots = %d -> %d, want 5 -> 11", old.rust, got.rust)
	}
	if got.highmem != old.highmem || got.r8 != old.r8 || got.java != old.java || got.kotlin != old.kotlin {
		t.Fatal("unrelated pools changed")
	}
	if explicit := admission.decide(18, snapshot, []string{"NINJA_UNI_RUST_NUM_JOBS=3"}, 1); explicit.rust != 3 {
		t.Fatal("explicit Rust pool limit changed")
	}
}
