// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type activeBuildLease struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	Scope     string `json:"scope,omitempty"`
	Token     string `json:"token"`
}

func activeBuildLeasePath(outDir string) string {
	return filepath.Join(outDir, "uni", ".active-build.json")
}

func writeActiveBuildLease(outDir string, pid int, scope string) (activeBuildLease, error) {
	identity, err := readProcessIdentity(pid)
	if err != nil {
		return activeBuildLease{}, err
	}
	return writeActiveBuildLeaseForProcess(outDir, identity, scope)
}

func writeActiveBuildLeaseForProcess(outDir string, identity processIdentity, scope string) (activeBuildLease, error) {
	if identity.PID <= 0 || identity.StartTime == 0 {
		return activeBuildLease{}, fmt.Errorf("invalid active build process identity")
	}
	lease := activeBuildLease{
		PID:       identity.PID,
		StartTime: identity.StartTime,
		Scope:     scope,
		Token:     fmt.Sprintf("%d-%d-%d", identity.PID, identity.StartTime, time.Now().UnixNano()),
	}
	data, err := json.Marshal(lease)
	if err != nil {
		return activeBuildLease{}, err
	}
	path := activeBuildLeasePath(outDir)
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return activeBuildLease{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".active-build-*")
	if err != nil {
		return activeBuildLease{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return activeBuildLease{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return activeBuildLease{}, err
	}
	if err := temporary.Close(); err != nil {
		return activeBuildLease{}, err
	}
	if err := os.Chmod(temporaryPath, 0664); err != nil {
		return activeBuildLease{}, err
	}
	if err := renameAndSync(temporaryPath, path); err != nil {
		return activeBuildLease{}, err
	}
	return lease, nil
}

func readActiveBuildLease(outDir string) (activeBuildLease, error) {
	data, err := os.ReadFile(activeBuildLeasePath(outDir))
	if err != nil {
		return activeBuildLease{}, err
	}
	var lease activeBuildLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return activeBuildLease{}, err
	}
	if lease.PID <= 0 || lease.StartTime == 0 || lease.Token == "" {
		return activeBuildLease{}, fmt.Errorf("invalid active build lease")
	}
	return lease, nil
}

func activeBuildStillRunning(outDir string) (string, error) {
	if pid := runningUniProcess(outDir); pid != 0 {
		return fmt.Sprintf("process %d", pid), nil
	}
	lease, err := readActiveBuildLease(outDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		if removeErr := os.Remove(activeBuildLeasePath(outDir)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", errors.Join(err, removeErr)
		}
		return "", nil
	}
	startTime, startErr := processStartTime(lease.PID)
	if startErr == nil && startTime == lease.StartTime {
		if lease.Scope != "" {
			return lease.Scope, nil
		}
		return fmt.Sprintf("process group %d", lease.PID), nil
	}
	if killErr := syscall.Kill(-lease.PID, 0); killErr == nil || errors.Is(killErr, syscall.EPERM) {
		return fmt.Sprintf("process group %d", lease.PID), nil
	}
	if err := os.Remove(activeBuildLeasePath(outDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return "", nil
}

func clearActiveBuildLease(outDir, token string) error {
	lease, err := readActiveBuildLease(outDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.Token != token {
		return nil
	}
	if err := os.Remove(activeBuildLeasePath(outDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
