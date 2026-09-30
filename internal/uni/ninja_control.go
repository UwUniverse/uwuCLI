// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ninjaParallelismFile struct {
	directory, path string
	ceiling         int
}

// Soong's command-line KEY=value assignments override the inherited environment.
// Keep both paths consistent, retaining all caller-supplied Ninja arguments.
func withParallelismFile(args, environment []string, path string) ([]string, string) {
	extra, _ := environmentValue(environment, "NINJA_EXTRA_ARGS")
	for _, arg := range args {
		if strings.HasPrefix(arg, "NINJA_EXTRA_ARGS=") {
			extra = strings.TrimPrefix(arg, "NINJA_EXTRA_ARGS=")
		}
	}
	extra = strings.TrimSpace(extra + " --parallelism-file " + path)
	result := append([]string(nil), args...)
	for index, arg := range result {
		if strings.HasPrefix(arg, "NINJA_EXTRA_ARGS=") {
			result[index] = "NINJA_EXTRA_ARGS=" + extra
		}
	}
	return result, extra
}

func prepareNinjaParallelismFile(binary string, jobs int) (*ninjaParallelismFile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	help, _ := exec.CommandContext(ctx, binary, "--help").CombinedOutput()
	if !strings.Contains(string(help), "--parallelism-file") {
		return nil, nil
	}
	directory, err := os.MkdirTemp("", "uni-admission-")
	if err != nil {
		return nil, err
	}
	control := &ninjaParallelismFile{directory: directory,
		path: filepath.Join(directory, "jobs"), ceiling: jobs}
	if _, err := control.setParallelism(ctx, jobs); err != nil {
		control.close()
		return nil, err
	}
	return control, nil
}

func (control *ninjaParallelismFile) setParallelism(ctx context.Context, jobs int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if jobs < 1 || jobs > control.ceiling {
		return "", fmt.Errorf("parallelism %d is outside 1..%d", jobs, control.ceiling)
	}
	// Atomic replacement prevents Ninja from observing a partial or empty limit.
	temporary := control.path + ".next"
	if err := os.WriteFile(temporary, []byte(fmt.Sprintf("%d\n", jobs)), 0600); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, control.path); err != nil {
		return "", err
	}
	return fmt.Sprintf("admission limit=%d", jobs), nil
}

func (control *ninjaParallelismFile) close() {
	if control != nil {
		_ = os.RemoveAll(control.directory)
	}
}
