// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type processIdentity struct {
	PID       int
	ParentPID int
	StartTime uint64
}

func readProcessIdentity(pid int) (processIdentity, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	closing := strings.LastIndex(string(data), ") ")
	if closing < 0 {
		return processIdentity{}, syscall.EINVAL
	}
	fields := strings.Fields(string(data)[closing+2:])
	if len(fields) <= 19 {
		return processIdentity{}, syscall.EINVAL
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return processIdentity{}, err
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{PID: pid, ParentPID: parentPID, StartTime: startTime}, nil
}

func processStartTime(pid int) (uint64, error) {
	identity, err := readProcessIdentity(pid)
	return identity.StartTime, err
}

func snapshotProcessTreeForRoot(root processIdentity) []processIdentity {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := make(map[int][]processIdentity)
	rootFound := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		identity, err := readProcessIdentity(pid)
		if err != nil {
			continue
		}
		if pid == root.PID && identity.StartTime == root.StartTime {
			rootFound = true
		}
		children[identity.ParentPID] = append(children[identity.ParentPID], identity)
	}
	if !rootFound {
		return nil
	}
	result := []processIdentity{root}
	for index := 0; index < len(result); index++ {
		result = append(result, children[result[index].PID]...)
	}
	return result
}

func snapshotProcessTree(rootPID int) []processIdentity {
	root, err := readProcessIdentity(rootPID)
	if err != nil {
		return nil
	}
	return snapshotProcessTreeForRoot(root)
}

func runningUniProcesses(outDir string) []processIdentity {
	return runningUniProcessesForBuild(outDir, true)
}

func isKotlinDaemonCommandLine(commandLine []byte) bool {
	for _, argument := range bytes.Split(commandLine, []byte{0}) {
		if string(argument) == "org.jetbrains.kotlin.daemon.KotlinCompileDaemon" {
			return true
		}
	}
	return false
}

func runningUniProcessesForBuild(outDir string, includeKotlinDaemon bool) []processIdentity {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	stateRoot := filepath.Clean(filepath.Join(outDir, "uni"))
	statePrefix := stateRoot + string(os.PathSeparator)
	var processes []processIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if !includeKotlinDaemon {
			commandLine, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if err == nil && isKotlinDaemonCommandLine(commandLine) {
				continue
			}
		}
		environment, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, variable := range bytes.Split(environment, []byte{0}) {
			const prefix = "UNI_STATE_FILE="
			if !bytes.HasPrefix(variable, []byte(prefix)) {
				continue
			}
			statePath := filepath.Clean(string(variable[len(prefix):]))
			if statePath == stateRoot || strings.HasPrefix(statePath, statePrefix) {
				identity, err := readProcessIdentity(pid)
				if err == nil {
					processes = append(processes, identity)
				}
				break
			}
		}
	}
	return processes
}

func runningUniProcess(outDir string) int {
	processes := runningUniProcessesForBuild(outDir, false)
	if len(processes) == 0 {
		return 0
	}
	return processes[0].PID
}

func sameProcess(identity processIdentity) bool {
	current, err := readProcessIdentity(identity.PID)
	return err == nil && current.StartTime == identity.StartTime
}

func signalProcessIdentities(processes []processIdentity, signal syscall.Signal) {
	for _, process := range processes {
		if sameProcess(process) {
			_ = syscall.Kill(process.PID, signal)
		}
	}
}

func signalProcessTree(rootPID int, signal syscall.Signal) []processIdentity {
	root, err := readProcessIdentity(rootPID)
	if err != nil {
		return nil
	}
	return signalProcessTreeIdentity(root, signal)
}

func signalProcessTreeIdentity(root processIdentity, signal syscall.Signal) []processIdentity {
	processes := snapshotProcessTreeForRoot(root)
	if sameProcess(root) {
		_ = syscall.Kill(-root.PID, signal)
	}
	signalProcessIdentities(processes, signal)
	return processes
}

func mergeProcessIdentities(first, second []processIdentity) []processIdentity {
	seen := make(map[[2]uint64]struct{}, len(first)+len(second))
	merged := make([]processIdentity, 0, len(first)+len(second))
	for _, list := range [][]processIdentity{first, second} {
		for _, process := range list {
			key := [2]uint64{uint64(process.PID), process.StartTime}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, process)
		}
	}
	return merged
}

func waitForProcessTreeExit(rootPID int, processes []processIdentity, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, process := range processes {
			if sameProcess(process) {
				alive = true
				break
			}
		}
		if !alive {
			err := syscall.Kill(-rootPID, 0)
			alive = err == nil || errors.Is(err, syscall.EPERM)
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitForProcessIdentitiesExit(processes []processIdentity, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, process := range processes {
			if sameProcess(process) {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
