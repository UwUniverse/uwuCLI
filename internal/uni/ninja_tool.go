// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	command := exec.Command(binaryPath, "-d", "list")
	output, _ := command.CombinedOutput()
	if !bytes.Contains(output, []byte("assumeexisting")) {
		return fmt.Errorf("custom Ninja does not expose assumeexisting mode")
	}
	return nil
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
	fmt.Printf("uni: build assume-existing Ninja\n")
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
