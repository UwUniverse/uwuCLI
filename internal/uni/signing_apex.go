// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var apexKeyEntry = regexp.MustCompile(`^name="([^"]+)"\s+public_key="([^"]+)"\s+private_key="([^"]+)"\s+container_certificate="([^"]+)"\s+container_private_key="([^"]+)"(?:\s+partition="[^"]*")?(?:\s+sign_tool="[^"]*")?$`)
var apexFileName = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(?:apex|capex)$`)

func apexesToSign(targetFiles string) ([]string, error) {
	archive, err := zip.OpenReader(targetFiles)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	var metadata *zip.File
	for _, file := range archive.File {
		if file.Name == "META/apexkeys.txt" {
			metadata = file
			break
		}
	}
	if metadata == nil {
		return nil, nil
	}
	if metadata.UncompressedSize64 > 8*1024*1024 {
		return nil, fmt.Errorf("META/apexkeys.txt is unexpectedly large")
	}
	reader, err := metadata.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, 8*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8*1024*1024 {
		return nil, fmt.Errorf("META/apexkeys.txt is unexpectedly large")
	}
	var names []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := apexKeyEntry.FindStringSubmatch(line)
		if fields == nil || !apexFileName.MatchString(fields[1]) {
			return nil, fmt.Errorf("invalid APEX signing entry: %q", line)
		}
		name := fields[1]
		if seen[name] {
			return nil, fmt.Errorf("duplicate APEX signing entry: %s", name)
		}
		seen[name] = true
		presigned := fields[3] == "PRESIGNED" || fields[4] == "PRESIGNED" || fields[5] == "PRESIGNED"
		if presigned {
			if fields[3] != "PRESIGNED" || fields[4] != "PRESIGNED" || fields[5] != "PRESIGNED" {
				return nil, fmt.Errorf("mixed PRESIGNED and signing keys for APEX %s", name)
			}
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func managedSigningKeys(keysDir string) (bool, error) {
	marker := filepath.Join(keysDir, managedSigningKeysMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("invalid Uni signing key marker: %s", marker)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return false, err
	}
	if string(data) != "v1\n" {
		return false, fmt.Errorf("unsupported Uni signing key marker: %s", marker)
	}
	return true, nil
}

func existingApexSigningKey(directory string) (string, error) {
	base := filepath.Join(directory, "key")
	if err := requireSigningKey(directory, "key"); err != nil {
		return "", err
	}
	info, err := os.Stat(base + ".pem")
	if err != nil || info.Size() == 0 {
		return "", fmt.Errorf("missing APEX payload signing key %s.pem", base)
	}
	return base, nil
}

func ensureApexSigningKey(ctx context.Context, keysDir, name string) (string, error) {
	root := filepath.Join(keysDir, "apex")
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid APEX signing key directory: %s", root)
	}
	directory := filepath.Join(root, name)
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("invalid APEX signing key directory: %s", directory)
		}
		return existingApexSigningKey(directory)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(root, ".uni-apex-key-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := createSigningKeyPair(staging, "key", 4096, true); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", fmt.Errorf("APEX signing key directory already exists or cannot be reserved: %w", err)
	}
	installed := make([]string, 0, 3)
	for _, suffix := range []string{".pk8", ".x509.pem", ".pem"} {
		file := "key" + suffix
		path := filepath.Join(directory, file)
		if err := os.Link(filepath.Join(staging, file), path); err != nil {
			for _, created := range installed {
				_ = os.Remove(created)
			}
			_ = os.Remove(directory)
			return "", fmt.Errorf("install APEX signing key %s: %w", name, err)
		}
		installed = append(installed, path)
	}
	fmt.Printf("uni: initialized APEX signing key: %s\n", name)
	return filepath.Join(directory, "key"), nil
}

func managedApexSigningMappings(ctx context.Context, targetFiles, keysDir string) (map[string]string, map[string]string, error) {
	managed, err := managedSigningKeys(keysDir)
	if err != nil || !managed {
		return nil, nil, err
	}
	names, err := apexesToSign(targetFiles)
	if err != nil {
		return nil, nil, err
	}
	containers := make(map[string]string, len(names))
	payloads := make(map[string]string, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		base, err := ensureApexSigningKey(ctx, keysDir, name)
		if err != nil {
			return nil, nil, err
		}
		containers[name] = base
		payloads[name] = base + ".pem"
	}
	return containers, payloads, nil
}
