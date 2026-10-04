// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type signingConfig struct {
	KeyMappings         map[string]string `json:"key_mappings"`
	ExtraAPKs           map[string]string `json:"extra_apks"`
	ExtraApexPayloadKey map[string]string `json:"extra_apex_payload_keys"`
}

type signingTools struct {
	signTargetFiles  string
	otaFromTarget    string
	imgFromTarget    string
	buildSuperImage  string
	workingDirectory string
}

type signingResult struct {
	SourceTargetFiles string
	SignedTargetFiles string
	SignedOTA         string
	SignedFastboot    string
	Checksum          string
	Check             bool
}

func signingBuildOptions(options Options) (Options, error) {
	for _, target := range options.Targets {
		if target == "otapackage" {
			continue
		}
		if target == "updatepackage" &&
			(options.PackageMode == packageModeFastboot || options.PackageMode == packageModeBoth) {
			continue
		}
		return Options{}, fmt.Errorf("--sign-keys supports otapackage, uwu, and uwu-f")
	}
	args := make([]string, 0, len(options.BuildArgs)+2)
	for _, arg := range options.BuildArgs {
		if arg != "otapackage" && arg != "updatepackage" {
			args = append(args, arg)
		}
	}
	options.BuildArgs = append(args, "target-files-package", "otatools")
	options.Targets = []string{"target-files-package", "otatools"}
	options.FullBuild = true
	if options.PackageMode == "" {
		options.PackageMode = packageModeOTA
	}
	return options, nil
}

func applyVendorPrivateKeySigningPolicy(top string, options Options) (Options, error) {
	if options.SignKeys == "" {
		return options, nil
	}
	privateKeysDir := filepath.Join(top, "vendor", "uwu-priv", "keys")
	info, err := os.Stat(privateKeysDir)
	if os.IsNotExist(err) {
		return options, nil
	}
	if err != nil {
		return options, fmt.Errorf("check vendor private signing keys %s: %w", privateKeysDir, err)
	}
	if !info.IsDir() {
		return options, nil
	}
	if options.SignCheck {
		return options, fmt.Errorf("vendor private signing keys exist at %s; Uni signing checks are disabled because the build system already signs OTA packages", privateKeysDir)
	}

	switch options.PackageMode {
	case "", packageModeOTA:
		buildArgs := make([]string, 0, len(options.BuildArgs))
		for _, arg := range options.BuildArgs {
			if arg != "target-files-package" && arg != "otatools" {
				buildArgs = append(buildArgs, arg)
			}
		}
		options.BuildArgs = append(buildArgs, "otapackage")
		options.Targets = []string{"otapackage"}
		options.PackageMode = packageModeOTA
		options.SignKeys = ""
		options.SignConfig = ""
		options.SignPath = ""
		fmt.Printf("uni: %s exists; using build-system OTA signing and skipping Uni OTA signing\n", privateKeysDir)
	case packageModeBoth:
		options.BuildArgs = append(options.BuildArgs, "otapackage")
		options.Targets = append(options.Targets, "otapackage")
		options.SkipOTASigning = true
		fmt.Printf("uni: %s exists; using build-system OTA signing and keeping Uni fastboot signing only\n", privateKeysDir)
	case packageModeFastboot:
		return options, nil
	default:
		return options, fmt.Errorf("unknown package mode %q", options.PackageMode)
	}
	return options, nil
}

func signingToolPaths(top, outDir string) signingTools {
	bin := filepath.Join(outDir, "host", "linux-x86", "bin")
	return signingTools{
		signTargetFiles:  filepath.Join(bin, "sign_target_files_apks"),
		otaFromTarget:    filepath.Join(bin, "ota_from_target_files"),
		imgFromTarget:    filepath.Join(bin, "img_from_target_files"),
		buildSuperImage:  filepath.Join(bin, "build_super_image"),
		workingDirectory: top,
	}
}

func outDirectoryEmpty(outDir string) (bool, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

func resolveSigningOutputDirectory(top, outDir, signPath string, check bool) (string, error) {
	directory := strings.TrimSpace(signPath)
	if directory == "" {
		empty, err := outDirectoryEmpty(outDir)
		if err != nil {
			return "", err
		}
		if empty {
			directory = outDir
		} else {
			directory = filepath.Join(top, "Sign.Out")
		}
	}
	if !filepath.IsAbs(directory) {
		absolute, err := filepath.Abs(directory)
		if err != nil {
			return "", err
		}
		directory = absolute
	}
	if check {
		directory = filepath.Join(directory, "checks", time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(directory, 0777); err != nil {
		return "", err
	}
	return directory, nil
}

func signingArtifactNames(directory, product string) (string, string) {
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(directory, product+"-target_files-signed-"+stamp+".zip"),
		filepath.Join(directory, product+"-ota-signed-"+stamp+".zip")
}

func findTargetFiles(productOut string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(productOut, "obj", "PACKAGING", "target_files_intermediates", "*target_files*.zip"))
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("no target-files package found under %s", productOut)
	}
	if len(paths) != 1 {
		sort.Strings(paths)
		return "", fmt.Errorf("multiple target-files packages found; keep only the intended package: %s", strings.Join(paths, ", "))
	}
	return paths[0], nil
}

func requireSigningKey(keysDir, name string) error {
	base := filepath.Join(keysDir, name)
	for _, suffix := range []string{".pk8", ".x509.pem"} {
		if info, err := os.Stat(base + suffix); err != nil || info.IsDir() {
			return fmt.Errorf("missing signing key %s%s", name, suffix)
		}
	}
	return nil
}

func validateSigningKeysDirectory(keysDir string) (string, error) {
	if keysDir == "" {
		return "", fmt.Errorf("signing keys directory is empty")
	}
	absolute, err := filepath.Abs(keysDir)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return "", fmt.Errorf("signing keys directory does not exist: %s", absolute)
	}
	for _, name := range []string{"releasekey", "platform", "shared", "media"} {
		if err := requireSigningKey(absolute, name); err != nil {
			return "", err
		}
	}
	return absolute, nil
}

func verifyTargetFiles(targetFiles string) error {
	archive, err := zip.OpenReader(targetFiles)
	if err != nil {
		return fmt.Errorf("open target-files package: %w", err)
	}
	defer archive.Close()
	required := map[string]bool{
		"META/apkcerts.txt":  false,
		"META/misc_info.txt": false,
	}
	for _, file := range archive.File {
		if _, found := required[file.Name]; found {
			required[file.Name] = true
		}
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("target-files package is missing %s", name)
		}
	}
	return nil
}

func loadSigningConfig(path, keysDir string) (signingConfig, error) {
	if path == "" {
		return signingConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return signingConfig{}, fmt.Errorf("read signing config: %w", err)
	}
	var config signingConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return signingConfig{}, fmt.Errorf("parse signing config: %w", err)
	}
	resolve := func(entries map[string]string) {
		for name, key := range entries {
			if key != "" && !filepath.IsAbs(key) {
				entries[name] = filepath.Join(keysDir, key)
			}
		}
	}
	resolve(config.KeyMappings)
	resolve(config.ExtraAPKs)
	resolve(config.ExtraApexPayloadKey)
	return config, nil
}

func appendSortedSigningMappings(args []string, flag string, entries map[string]string) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if entries[name] != "" {
			args = append(args, flag, name+"="+entries[name])
		}
	}
	return args
}

func signingCommand(ctx context.Context, path, workingDirectory string, args ...string) error {
	command := exec.CommandContext(ctx, path, args...)
	if workingDirectory != "" {
		command.Dir = workingDirectory
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeSigningChecksum(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return "", err
	}
	checksum := path + ".sha256"
	line := fmt.Sprintf("%x  %s\n", hash.Sum(nil), filepath.Base(path))
	if err := os.WriteFile(checksum, []byte(line), 0644); err != nil {
		return "", err
	}
	return checksum, nil
}

func signTargetFiles(ctx context.Context, targetFiles, keysDir, configPath, outputDir, product string, generateOTA, check bool, tools signingTools) (signingResult, error) {
	keysDir, err := validateSigningKeysDirectory(keysDir)
	if err != nil {
		return signingResult{}, err
	}
	if err := verifyTargetFiles(targetFiles); err != nil {
		return signingResult{}, err
	}
	requiredTools := []string{tools.signTargetFiles}
	if generateOTA {
		requiredTools = append(requiredTools, tools.otaFromTarget)
	}
	for _, tool := range requiredTools {
		if info, err := os.Stat(tool); err != nil || info.Mode()&0111 == 0 {
			return signingResult{}, fmt.Errorf("missing executable signing tool: %s", tool)
		}
	}
	config, err := loadSigningConfig(configPath, keysDir)
	if err != nil {
		return signingResult{}, err
	}
	autoAPKs, autoPayloads, err := managedApexSigningMappings(ctx, targetFiles, keysDir)
	if err != nil {
		return signingResult{}, fmt.Errorf("prepare APEX signing keys: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0777); err != nil {
		return signingResult{}, err
	}
	signedTargetFiles, signedOTA := signingArtifactNames(outputDir, product)
	args := []string{"-o", "-d", keysDir}
	args = appendSortedSigningMappings(args, "-k", config.KeyMappings)
	args = appendSortedSigningMappings(args, "--extra_apks", autoAPKs)
	args = appendSortedSigningMappings(args, "--extra_apex_payload_key", autoPayloads)
	args = appendSortedSigningMappings(args, "--extra_apks", config.ExtraAPKs)
	args = appendSortedSigningMappings(args, "--extra_apex_payload_key", config.ExtraApexPayloadKey)
	args = append(args, targetFiles, signedTargetFiles)
	if err := signingCommand(ctx, tools.signTargetFiles, tools.workingDirectory, args...); err != nil {
		return signingResult{}, err
	}
	artifacts := []string{signedTargetFiles}
	if generateOTA {
		if err := signingCommand(ctx, tools.otaFromTarget, tools.workingDirectory, "-k", filepath.Join(keysDir, "releasekey"), signedTargetFiles, signedOTA); err != nil {
			return signingResult{}, err
		}
		artifacts = append(artifacts, signedOTA)
	} else {
		signedOTA = ""
	}
	for _, artifact := range artifacts {
		if info, err := os.Stat(artifact); err != nil || info.Size() == 0 {
			return signingResult{}, fmt.Errorf("signing tool did not create %s", artifact)
		}
	}
	checksum := ""
	if generateOTA {
		checksum, err = writeSigningChecksum(signedOTA)
		if err != nil {
			return signingResult{}, fmt.Errorf("write OTA checksum: %w", err)
		}
	}
	return signingResult{SourceTargetFiles: targetFiles, SignedTargetFiles: signedTargetFiles, SignedOTA: signedOTA, Checksum: checksum, Check: check}, nil
}

func buildSignedFastbootPackage(ctx context.Context, signedTargetFiles, outputDir, product string, tools signingTools) (string, error) {
	for _, tool := range []string{tools.imgFromTarget, tools.buildSuperImage} {
		if info, err := os.Stat(tool); err != nil || info.Mode()&0111 == 0 {
			return "", fmt.Errorf("missing executable signing tool: %s", tool)
		}
	}
	output := filepath.Join(outputDir, product+"-img-signed-"+time.Now().Format("20060102-150405")+".zip")
	args := []string{
		"--additional", "IMAGES/VerifiedBootParams.textproto:VerifiedBootParams.textproto",
		"--build_super_image", tools.buildSuperImage,
		signedTargetFiles, output,
	}
	if err := signingCommand(ctx, tools.imgFromTarget, tools.workingDirectory, args...); err != nil {
		return "", err
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		return "", fmt.Errorf("fastboot packaging tool did not create %s", output)
	}
	return output, nil
}

func runSigning(ctx context.Context, top, outDir string, state State, options Options) (signingResult, error) {
	targetFiles, err := findTargetFiles(state.ProductOut)
	if err != nil {
		return signingResult{}, err
	}
	outputDir, err := resolveSigningOutputDirectory(top, outDir, options.SignPath, options.SignCheck)
	if err != nil {
		return signingResult{}, err
	}
	fmt.Printf("uni: signing output: %s\n", outputDir)
	modes, err := packageModes(options.PackageMode)
	if err != nil {
		return signingResult{}, err
	}
	generateOTA := len(modes) == 0
	generateFastboot := false
	for _, mode := range modes {
		generateOTA = generateOTA || mode == packageModeOTA
		generateFastboot = generateFastboot || mode == packageModeFastboot
	}
	if options.SkipOTASigning {
		generateOTA = false
	}
	tools := signingToolPaths(top, outDir)
	result, err := signTargetFiles(ctx, targetFiles, options.SignKeys, options.SignConfig, outputDir,
		state.TargetProduct, generateOTA, options.SignCheck, tools)
	if err != nil {
		return signingResult{}, err
	}
	if generateFastboot {
		result.SignedFastboot, err = buildSignedFastbootPackage(ctx, result.SignedTargetFiles, outputDir, state.TargetProduct, tools)
		if err != nil {
			return signingResult{}, err
		}
	}
	return result, nil
}
