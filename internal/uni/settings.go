// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"errors"
	"os"
	"path/filepath"
)

func devAutoSettingPath(top string) string {
	return filepath.Join(top, ".repo", "uwuCLI", "dev_auto")
}

func loadDevAutoSetting(top string) (bool, error) {
	_, err := os.Stat(devAutoSettingPath(top))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func saveDevAutoSetting(top string, enabled bool) error {
	path := devAutoSettingPath(top)
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dev-auto-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString("1\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0666); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
