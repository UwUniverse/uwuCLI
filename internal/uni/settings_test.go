// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDevAutoSettingPersistsOutsideOut(t *testing.T) {
	top := t.TempDir()
	out := filepath.Join(top, "out")
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveDevAutoSetting(top, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	if enabled, err := loadDevAutoSetting(top); err != nil || !enabled {
		t.Fatalf("enabled=%t err=%v", enabled, err)
	}
	if err := saveDevAutoSetting(top, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := loadDevAutoSetting(top); err != nil || enabled {
		t.Fatalf("enabled=%t err=%v", enabled, err)
	}
}
