// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func ninjaToolSourceNewer(sourceDir, binaryPath string) (bool, error) {
	binaryInfo, err := os.Stat(binaryPath)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	newer := false
	err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		extension := filepath.Ext(path)
		if extension != ".cc" && extension != ".h" && extension != ".py" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(binaryInfo.ModTime()) {
			newer = true
			return fs.SkipAll
		}
		return nil
	})
	return newer, err
}

func verifyAssumeExistingNinja(binaryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, "-d", "list")
	output, _ := command.CombinedOutput()
	if !bytes.Contains(output, []byte("assumeexisting")) ||
		!bytes.Contains(output, []byte("except API validation outputs")) {
		return fmt.Errorf("executor does not expose API-safe assumeexisting mode")
	}
	return nil
}

func (runner *commandRunner) prepareAssumeExistingExecutor(outDir string) (string, error) {
	executor := runner.phasedNinja
	if configured, _ := environmentValue(runner.baseEnv, "UNI_NINJA_BIN"); configured != "" {
		executor = configured
	}
	if executor != "ninja" {
		if binary, err := resolveExecutorPath(executor, runner.top); err == nil {
			if verifyAssumeExistingNinja(binary) == nil {
				return binary, nil
			}
		}
	}
	return ensureAssumeExistingNinja(runner.top, outDir)
}

func ensureAssumeExistingNinja(top, outDir string) (string, error) {
	sourceDir := filepath.Join(top, "external", "ninja")
	configurePath := filepath.Join(sourceDir, "configure.py")
	if _, err := os.Stat(configurePath); err != nil {
		return "", fmt.Errorf("custom Ninja source is unavailable: %w", err)
	}
	buildDir := filepath.Join(outDir, "uwuCLI", "ninja-assume-existing")
	builtBinary := filepath.Join(buildDir, "ninja")
	binaryPath := filepath.Join(outDir, "uwuCLI", "bin", "ninja-assume-existing")
	rebuild, err := ninjaToolSourceNewer(sourceDir, binaryPath)
	if err != nil {
		return "", err
	}
	if !rebuild {
		if err := verifyAssumeExistingNinja(binaryPath); err == nil {
			return binaryPath, nil
		}
	}
	if err := os.MkdirAll(buildDir, 0777); err != nil {
		return "", err
	}
	pythonPath := filepath.Join(top, "prebuilts", "build-tools", "path", "linux-x86", "python3")
	compilerPath := filepath.Join(top, "prebuilts", "clang", "host", "linux-x86", "clang-r584948b", "bin", "clang++")
	archiverPath := filepath.Join(filepath.Dir(compilerPath), "llvm-ar")
	for _, path := range []string{pythonPath, compilerPath, archiverPath} {
		if info, err := os.Stat(path); err != nil || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("required Ninja build tool is unavailable: %s", path)
		}
	}
	fmt.Printf("uni: build Ninja compatibility executor\n")
	command := exec.Command(pythonPath, configurePath, "--bootstrap", "--with-python="+pythonPath)
	command.Dir = buildDir
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = overrideEnvironment(os.Environ(),
		"CXX="+compilerPath,
		"AR="+archiverPath,
		"CXXFLAGS="+strings.TrimSpace(os.Getenv("CXXFLAGS")))
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("build assume-existing Ninja: %w", err)
	}
	if err := verifyAssumeExistingNinja(builtBinary); err != nil {
		return "", err
	}
	if err := cloneOrCopyFileAtomic(builtBinary, binaryPath); err != nil {
		return "", err
	}
	return binaryPath, nil
}
