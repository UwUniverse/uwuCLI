/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package uni

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	runaControlTimeout = 2 * time.Second
)

type runaControlSession struct {
	directory string
	binary    string
	wrapper   string
	socket    string
}

type runaStatus struct {
	Parallelism         int
	OriginalParallelism int
	Running             int
	Retrying            int
	SuccessfulActions   uint64
}

func prepareRunaControl(executor, top string) (*runaControlSession, string, error) {
	if runtime.GOOS != "linux" {
		return nil, "runtime control requires Linux Unix sockets", nil
	}
	if executor == "" {
		return nil, "no Ninja executor selected", nil
	}
	binary, err := resolveExecutorPath(executor, top)
	if err != nil {
		return nil, fmt.Sprintf("cannot locate executor %q: %v", executor, err), nil
	}
	helpCtx, cancelHelp := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHelp()
	help, helpErr := exec.CommandContext(helpCtx, binary, "--help").CombinedOutput()
	if !strings.Contains(string(help), "--control-socket") {
		reason := fmt.Sprintf("executor %q does not advertise Runa runtime control", binary)
		if helpErr != nil {
			reason += ": " + helpErr.Error()
		}
		return nil, reason, nil
	}

	directory, err := os.MkdirTemp("", "uni-runa-")
	if err != nil {
		return nil, "", fmt.Errorf("create private Runa control directory: %w", err)
	}
	socket := filepath.Join(directory, "control.sock")
	if len(socket) >= 108 {
		_ = os.RemoveAll(directory)
		return nil, fmt.Sprintf("Runa socket path is too long (%d bytes)", len(socket)), nil
	}
	wrapper := filepath.Join(directory, "ninja-wrapper")
	contents := "#!/bin/sh\nexec " + shellQuote(binary) + " --control-socket " + shellQuote(socket) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(contents), 0700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, "", fmt.Errorf("write Runa executor wrapper: %w", err)
	}
	return &runaControlSession{directory: directory, binary: binary, wrapper: wrapper, socket: socket}, "", nil
}

func resolveExecutorPath(executor, top string) (string, error) {
	if executor == "runa" {
		if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
			prebuiltRuna := filepath.Join(top, "vendor", "uwu-prebuilts", "runa", "runa")
			if info, err := os.Stat(prebuiltRuna); err == nil && info.Mode()&0111 != 0 {
				return filepath.Abs(prebuiltRuna)
			}
		}
		builtRuna := filepath.Join(top, "out", "host", "linux-x86", "bin", "runa")
		if outDir, err := outputDirectory(top); err == nil {
			builtRuna = filepath.Join(outDir, "host", "linux-x86", "bin", "runa")
		}
		if info, err := os.Stat(builtRuna); err == nil && info.Mode()&0111 != 0 {
			return filepath.Abs(builtRuna)
		}
	}
	if executor == "ninja" {
		tag := "linux-x86"
		binDirectory := "bin"
		if environmentTrue(os.Environ(), "SANITIZE_BUILD_TOOL_PREBUILTS") {
			binDirectory = filepath.Join("asan", "bin")
		}
		prebuilt := filepath.Join(top, "prebuilts", "build-tools", tag, binDirectory, "ninja")
		if info, err := os.Stat(prebuilt); err == nil && info.Mode()&0111 != 0 {
			return filepath.Abs(prebuilt)
		}
	}
	if strings.ContainsRune(executor, filepath.Separator) {
		path := executor
		if !filepath.IsAbs(path) {
			path = filepath.Join(top, path)
		}
		return filepath.Abs(path)
	}
	return exec.LookPath(executor)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (session *runaControlSession) close() {
	if session != nil && session.directory != "" {
		_ = os.RemoveAll(session.directory)
	}
}

func (session *runaControlSession) request(ctx context.Context, request string) (string, error) {
	if session == nil {
		return "", fmt.Errorf("Runa control session is unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, runaControlTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(requestCtx, "unix", session.socket)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(runaControlTimeout)); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintln(connection, request); err != nil {
		return "", err
	}
	response, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(response), nil
}

func (session *runaControlSession) status(ctx context.Context) (runaStatus, error) {
	response, err := session.request(ctx, "get_status")
	if err != nil {
		return runaStatus{}, err
	}
	fields := strings.Fields(response)
	if len(fields) == 0 || fields[0] != "ok" {
		return runaStatus{}, fmt.Errorf("unexpected get_status response %q", response)
	}
	values := make(map[string]string, len(fields)-1)
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			values[key] = value
		}
	}
	parseInt := func(name string) (int, error) {
		value, err := strconv.Atoi(values[name])
		if err != nil {
			return 0, fmt.Errorf("parse Runa status %s: %w", name, err)
		}
		return value, nil
	}
	status := runaStatus{}
	if status.Parallelism, err = parseInt("parallelism"); err != nil {
		return runaStatus{}, err
	}
	if status.OriginalParallelism, err = parseInt("original_parallelism"); err != nil {
		return runaStatus{}, err
	}
	if status.Running, err = parseInt("running"); err != nil {
		return runaStatus{}, err
	}
	if status.Retrying, err = parseInt("retrying"); err != nil {
		return runaStatus{}, err
	}
	status.SuccessfulActions, err = strconv.ParseUint(values["successful_actions"], 10, 64)
	if err != nil {
		return runaStatus{}, fmt.Errorf("parse Runa status successful_actions: %w", err)
	}
	return status, nil
}

func (session *runaControlSession) setParallelism(ctx context.Context, jobs int) (string, error) {
	response, err := session.request(ctx, fmt.Sprintf("set_parallelism %d", jobs))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(response, fmt.Sprintf("ok parallelism=%d", jobs)) {
		return response, fmt.Errorf("unexpected set_parallelism response %q", response)
	}
	return response, nil
}

func (session *runaControlSession) cancelActionForRetry(ctx context.Context) (string, error) {
	response, err := session.request(ctx, "cancel_action_for_retry")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(response, "ok state=accepted") || strings.HasPrefix(response, "reject reason=no_retryable_action") {
		return response, nil
	}
	return response, fmt.Errorf("unexpected cancel_action_for_retry response %q", response)
}
