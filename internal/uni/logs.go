// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const automaticDebugReportLimit = 8
const automaticOutputLogLimit = 8

type logCleanup struct {
	files int
	bytes int64
}

func removeLogPath(path string) (logCleanup, error) {
	var cleaned logCleanup
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		cleaned.files++
		cleaned.bytes += info.Size()
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return logCleanup{}, nil
	}
	if err != nil {
		return logCleanup{}, err
	}
	if err := os.RemoveAll(path); err != nil {
		return logCleanup{}, err
	}
	return cleaned, nil
}

func removeLogPaths(paths []string) (logCleanup, error) {
	var total logCleanup
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		cleaned, err := removeLogPath(path)
		if err != nil {
			return total, err
		}
		total.files += cleaned.files
		total.bytes += cleaned.bytes
	}
	return total, nil
}

func pruneDebugReports(outDir string, keep int) (logCleanup, error) {
	return pruneLogs(outDir, "uwuCli-debug-report_*.log", keep)
}

func pruneOutputLogs(outDir string, keep int) (logCleanup, error) {
	return pruneLogs(outDir, "uwuCli-output_*.log", keep)
}

func pruneLogs(outDir, pattern string, keep int) (logCleanup, error) {
	matches, err := filepath.Glob(filepath.Join(outDir, pattern))
	if err != nil {
		return logCleanup{}, err
	}
	sort.Strings(matches)
	if keep < 0 {
		keep = 0
	}
	if len(matches) <= keep {
		return logCleanup{}, nil
	}
	return removeLogPaths(matches[:len(matches)-keep])
}

func cleanBuildLogs(outDir string) (logCleanup, error) {
	patterns := []string{
		"uwuCli-debug-report_*.log",
		"uwuCli-output_*.log",
		"verbose.log*",
		"error.log*",
		"soong.log*",
		"build.trace*.gz",
		"build_error*",
		"build_progress.pb*",
		"soong_metrics*",
		"soong_build_metrics*.pb",
		"execution_metrics*.pb",
		"rbe_metrics*.pb",
		"soong/siso.INFO*",
		"soong/siso_output",
		"soong/siso_failed_commands.sh",
	}
	var paths []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(outDir, pattern))
		if err != nil {
			return logCleanup{}, err
		}
		paths = append(paths, matches...)
	}
	if info, err := os.Stat(filepath.Join(outDir, "dist", "logs")); err == nil && info.IsDir() {
		paths = append(paths, filepath.Join(outDir, "dist", "logs"))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return logCleanup{}, err
	}
	return removeLogPaths(paths)
}
