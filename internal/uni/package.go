// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type releasePackageMetadata struct {
	version     string
	date        string
	releaseType string
	device      string
	signed      bool
}

type publishedPackage struct {
	path     string
	checksum string
}

func readProperties(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	properties := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return properties, nil
}

func releaseMetadata(productOut string) (releasePackageMetadata, error) {
	path := filepath.Join(productOut, "product", "etc", "build.prop")
	properties, err := readProperties(path)
	if err != nil {
		return releasePackageMetadata{}, fmt.Errorf("read release metadata from %s: %w", path, err)
	}
	version := properties["ro.uwu.release"]
	if version == "" {
		version = properties["ro.uwu.build.version"]
	}
	dateUTC := properties["ro.product.build.date.utc"]
	if dateUTC == "" {
		dateUTC = properties["ro.build.date.utc"]
	}
	timestamp, err := strconv.ParseInt(dateUTC, 10, 64)
	if err != nil || timestamp <= 0 {
		return releasePackageMetadata{}, fmt.Errorf("invalid build date %q in %s", dateUTC, path)
	}
	metadata := releasePackageMetadata{
		version:     version,
		date:        time.Unix(timestamp, 0).UTC().Format("20060102"),
		releaseType: properties["ro.uwu.releasetype"],
		device:      properties["ro.uwu.device"],
		signed:      strings.Contains(properties["ro.build.tags"], "release-keys"),
	}
	for name, value := range map[string]string{
		"version": metadata.version, "release type": metadata.releaseType, "device": metadata.device,
	} {
		if err := validatePackageSegment(name, value); err != nil {
			return releasePackageMetadata{}, err
		}
	}
	return metadata, nil
}

func validatePackageSegment(name, value string) error {
	if value == "" {
		return fmt.Errorf("release %s is empty", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return fmt.Errorf("release %s %q contains an unsafe filename character", name, value)
	}
	return nil
}

func releasePackageName(metadata releasePackageMetadata, signed, fastboot bool) string {
	signing := "unsigned"
	if signed {
		signing = "signed"
	}
	if fastboot {
		return fmt.Sprintf("uwuAOSP-%s-%s-%s-%s-%s-fastboot.zip",
			metadata.version, metadata.date, metadata.releaseType, metadata.device, signing)
	}
	return fmt.Sprintf("uwuAOSP-%s-%s-%s-%s-%s.zip",
		metadata.version, metadata.date, metadata.releaseType, metadata.device, signing)
}

func relocateChecksum(source, packagePath string) (string, error) {
	if source == "" {
		return "", nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", fmt.Errorf("checksum file is empty: %s", source)
	}
	destination := packagePath + ".sha256"
	line := fmt.Sprintf("%s  %s\n", fields[0], filepath.Base(packagePath))
	if err := os.WriteFile(destination, []byte(line), 0644); err != nil {
		return "", err
	}
	if filepath.Clean(source) != filepath.Clean(destination) {
		if err := os.Remove(source); err != nil {
			return "", err
		}
	}
	return destination, nil
}

func packageModes(mode string) ([]string, error) {
	switch mode {
	case "":
		return nil, nil
	case packageModeOTA:
		return []string{packageModeOTA}, nil
	case packageModeFastboot:
		return []string{packageModeFastboot}, nil
	case packageModeBoth:
		return []string{packageModeOTA, packageModeFastboot}, nil
	default:
		return nil, fmt.Errorf("unknown release package mode %q", mode)
	}
}

func publishReleasePackage(state State, mode, signedPackage, checksum string) (publishedPackage, error) {
	metadata, err := releaseMetadata(state.ProductOut)
	if err != nil {
		return publishedPackage{}, err
	}

	fastboot := mode == packageModeFastboot
	source := signedPackage
	if source == "" {
		suffix := "-ota.zip"
		if fastboot {
			suffix = "-img.zip"
		}
		source = filepath.Join(state.ProductOut, state.TargetProduct+suffix)
	}
	info, err := os.Stat(source)
	if err != nil {
		return publishedPackage{}, fmt.Errorf("locate built package %s: %w", source, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return publishedPackage{}, fmt.Errorf("built package is empty or not a regular file: %s", source)
	}

	destination := filepath.Join(filepath.Dir(source), releasePackageName(metadata, metadata.signed || signedPackage != "", fastboot))
	if filepath.Clean(source) != filepath.Clean(destination) {
		if err := os.Rename(source, destination); err != nil {
			return publishedPackage{}, fmt.Errorf("rename release package: %w", err)
		}
	}
	publishedChecksum, err := relocateChecksum(checksum, destination)
	if err != nil {
		return publishedPackage{}, fmt.Errorf("rename release checksum: %w", err)
	}
	return publishedPackage{path: destination, checksum: publishedChecksum}, nil
}

func publishReleasePackages(state State, options Options, signedOTA, signedFastboot, checksum string) ([]publishedPackage, error) {
	modes, err := packageModes(options.PackageMode)
	if err != nil {
		return nil, err
	}
	packages := make([]publishedPackage, 0, len(modes))
	for _, mode := range modes {
		modeSignedPackage, modeChecksum := "", ""
		if mode == packageModeOTA {
			modeSignedPackage, modeChecksum = signedOTA, checksum
		} else if mode == packageModeFastboot {
			modeSignedPackage = signedFastboot
		}
		published, err := publishReleasePackage(state, mode, modeSignedPackage, modeChecksum)
		if err != nil {
			return nil, err
		}
		packages = append(packages, published)
	}
	return packages, nil
}
