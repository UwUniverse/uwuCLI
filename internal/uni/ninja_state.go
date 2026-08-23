// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	ninjaLogHeader   = "# ninja log v5"
	ninjaDepsHeader  = "# ninjadeps\n"
	ninjaDepsVersion = uint32(4)
	ficlone          = 0x40049409
)

type ninjaLogData struct {
	header string
	lines  map[string]string
	order  []string
}

func readNinjaLog(path string) (ninjaLogData, error) {
	data := ninjaLogData{lines: make(map[string]string)}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return data, nil
	}
	if err != nil {
		return data, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return data, err
		}
		return data, fmt.Errorf("%s has no Ninja log header", path)
	}
	if !strings.HasPrefix(scanner.Text(), "# ninja log v") {
		return data, fmt.Errorf("%s has an unsupported Ninja log header", path)
	}
	data.header = scanner.Text()
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) != 5 || fields[3] == "" {
			continue
		}
		output := fields[3]
		if _, exists := data.lines[output]; !exists {
			data.order = append(data.order, output)
		}
		data.lines[output] = line
	}
	if err := scanner.Err(); err != nil {
		return data, err
	}
	return data, nil
}

func mergeNinjaLogs(older, newer ninjaLogData) ninjaLogData {
	if older.header != "" && newer.header != "" && older.header != newer.header {
		return newer
	}
	merged := ninjaLogData{
		header: older.header,
		lines:  make(map[string]string, len(older.lines)+len(newer.lines)),
		order:  make([]string, 0, len(older.lines)+len(newer.lines)),
	}
	if newer.header != "" {
		merged.header = newer.header
	}
	appendData := func(data ninjaLogData) {
		for _, output := range data.order {
			if _, exists := merged.lines[output]; !exists {
				merged.order = append(merged.order, output)
			}
			merged.lines[output] = data.lines[output]
		}
	}
	appendData(older)
	appendData(newer)
	return merged
}

func filterNinjaLogByOutputs(data ninjaLogData, outDir string) ninjaLogData {
	filtered := ninjaLogData{
		header: data.header,
		lines:  make(map[string]string, len(data.lines)),
		order:  make([]string, 0, len(data.order)),
	}
	for _, output := range data.order {
		line := data.lines[output]
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) != 5 {
			continue
		}
		loggedMtime, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		path := output
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(outDir), filepath.FromSlash(path))
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				path = filepath.Join(outDir, filepath.FromSlash(output))
			}
		}
		info, err := os.Stat(path)
		if err != nil || info.ModTime().UnixNano() != loggedMtime {
			continue
		}
		filtered.order = append(filtered.order, output)
		filtered.lines[output] = line
	}
	return filtered
}

func writeNinjaLogAtomic(path string, data ninjaLogData) error {
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ninja-log-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	writer := bufio.NewWriterSize(temporary, 256*1024)
	header := data.header
	if header == "" {
		header = ninjaLogHeader
	}
	if _, err = fmt.Fprintln(writer, header); err == nil {
		for _, output := range data.order {
			if _, err = fmt.Fprintln(writer, data.lines[output]); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = writer.Flush()
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0664); err != nil {
		return err
	}
	return renameAndSync(temporaryPath, path)
}

func renameAndSync(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validNinjaDeps(path string) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, len(ninjaDepsHeader)+4)
	if _, err := io.ReadFull(file, header); err != nil {
		return false, nil
	}
	return string(header[:len(ninjaDepsHeader)]) == ninjaDepsHeader &&
		binary.LittleEndian.Uint32(header[len(ninjaDepsHeader):]) == ninjaDepsVersion, nil
}

func cloneOrCopyFileAtomic(source, destination string) error {
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0777); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".ninja-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	_, _, cloneErr := syscall.Syscall(syscall.SYS_IOCTL, temporary.Fd(), ficlone, sourceFile.Fd())
	if cloneErr != 0 {
		if err := temporary.Truncate(0); err != nil {
			temporary.Close()
			return err
		}
		if _, err := sourceFile.Seek(0, io.SeekStart); err != nil {
			temporary.Close()
			return err
		}
		if _, err := io.CopyBuffer(temporary, sourceFile, make([]byte, 1024*1024)); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chtimes(temporaryPath, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	return renameAndSync(temporaryPath, destination)
}

func filesEqual(first, second string) (bool, error) {
	firstInfo, err := os.Stat(first)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if firstInfo.Size() != secondInfo.Size() {
		return false, nil
	}

	firstFile, err := os.Open(first)
	if err != nil {
		return false, err
	}
	defer firstFile.Close()
	secondFile, err := os.Open(second)
	if err != nil {
		return false, err
	}
	defer secondFile.Close()

	firstBuffer := make([]byte, 256*1024)
	secondBuffer := make([]byte, len(firstBuffer))
	for {
		firstCount, firstErr := firstFile.Read(firstBuffer)
		secondCount, secondErr := secondFile.Read(secondBuffer)
		if firstCount != secondCount || !bytes.Equal(firstBuffer[:firstCount], secondBuffer[:secondCount]) {
			return false, nil
		}
		if errors.Is(firstErr, io.EOF) && errors.Is(secondErr, io.EOF) {
			return true, nil
		}
		if firstErr != nil {
			return false, firstErr
		}
		if secondErr != nil {
			return false, secondErr
		}
	}
}

func ninjaRecoveryDirectory(outDir string) string {
	return filepath.Join(outDir, "uni", ".ninja-state")
}

func ninjaRecoveryRequiredPath(outDir string) string {
	return filepath.Join(ninjaRecoveryDirectory(outDir), ".recovery-required")
}

func markNinjaRecoveryRequired(outDir string) error {
	path := ninjaRecoveryRequiredPath(outDir)
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0664)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func clearNinjaRecoveryRequired(outDir string) error {
	path := ninjaRecoveryRequiredPath(outDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ninjaRecoveryRequired(outDir string) (bool, error) {
	_, err := os.Stat(ninjaRecoveryRequiredPath(outDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func recoverNinjaLog(outDir string, forceMerge, trustOutput bool) error {
	currentPath := filepath.Join(outDir, ".ninja_log")
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_log")
	if !forceMerge {
		equal, err := filesEqual(currentPath, backupPath)
		if err != nil {
			return err
		}
		if equal {
			return nil
		}
	}
	backup, backupErr := readNinjaLog(backupPath)
	current, currentErr := readNinjaLog(currentPath)
	if backupErr != nil && currentErr != nil {
		return errors.Join(backupErr, currentErr)
	}
	if currentErr != nil && len(backup.lines) == 0 {
		return currentErr
	}
	if backupErr != nil && len(current.lines) == 0 {
		return backupErr
	}
	if backupErr != nil {
		backup = ninjaLogData{lines: make(map[string]string)}
	}
	if currentErr != nil {
		current = ninjaLogData{lines: make(map[string]string)}
	}
	if !trustOutput {
		backup = filterNinjaLogByOutputs(backup, outDir)
		current = filterNinjaLogByOutputs(current, outDir)
	}
	merged := mergeNinjaLogs(backup, current)
	if merged.header == "" {
		return nil
	}
	if err := writeNinjaLogAtomic(currentPath, merged); err != nil {
		return err
	}
	return writeNinjaLogAtomic(backupPath, merged)
}

func recoverNinjaDeps(outDir string) error {
	currentPath := filepath.Join(outDir, ".ninja_deps")
	backupPath := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_deps")
	backupValid, err := validNinjaDeps(backupPath)
	if err != nil {
		return err
	}
	currentValid, err := validNinjaDeps(currentPath)
	if err != nil {
		return err
	}
	if !backupValid {
		if !currentValid {
			return nil
		}
		return cloneOrCopyFileAtomic(currentPath, backupPath)
	}
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		return err
	}
	currentInfo, currentStatErr := os.Stat(currentPath)
	if !currentValid || currentStatErr != nil || currentInfo.Size() < backupInfo.Size() {
		return cloneOrCopyFileAtomic(backupPath, currentPath)
	}
	return nil
}

func checkpointNinjaDeps(outDir string) error {
	currentDeps := filepath.Join(outDir, ".ninja_deps")
	valid, err := validNinjaDeps(currentDeps)
	if err != nil {
		return err
	}
	if !valid {
		if _, statErr := os.Stat(currentDeps); errors.Is(statErr, os.ErrNotExist) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		return fmt.Errorf("%s has an invalid Ninja deps header", currentDeps)
	}
	backupDeps := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_deps")
	currentInfo, err := os.Stat(currentDeps)
	if err != nil {
		return err
	}
	backupInfo, backupErr := os.Stat(backupDeps)
	if backupErr == nil && currentInfo.Size() == backupInfo.Size() &&
		currentInfo.ModTime().Equal(backupInfo.ModTime()) {
		return nil
	}
	if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
		return backupErr
	}
	return cloneOrCopyFileAtomic(currentDeps, backupDeps)
}

func checkpointNinjaState(outDir string, interrupted, trustOutput bool) error {
	currentLog := filepath.Join(outDir, ".ninja_log")
	backupLog := filepath.Join(ninjaRecoveryDirectory(outDir), ".ninja_log")
	if interrupted {
		if err := recoverNinjaLog(outDir, true, trustOutput); err != nil {
			return err
		}
		if err := recoverNinjaDeps(outDir); err != nil {
			return err
		}
	} else if _, err := os.Stat(currentLog); err == nil {
		if err := cloneOrCopyFileAtomic(currentLog, backupLog); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return checkpointNinjaDeps(outDir)
}

func prepareNinjaState(outDir string, trustOutput bool) error {
	forceMerge, err := ninjaRecoveryRequired(outDir)
	if err != nil {
		return err
	}
	if err := recoverNinjaLog(outDir, forceMerge, trustOutput); err != nil {
		return err
	}
	if err := recoverNinjaDeps(outDir); err != nil {
		return err
	}
	return nil
}
