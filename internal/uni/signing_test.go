// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"archive/zip"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitializeSigningKeysAndReuse(t *testing.T) {
	top := t.TempDir()
	keysDir := filepath.Join(t.TempDir(), "android-certs")
	options, err := ParseOptions([]string{"--init-signing-keys", keysDir})
	if err != nil || options.InitSigningKeys != keysDir || options.FullBuild {
		t.Fatalf("invalid initialization options: %+v, %v", options, err)
	}
	actual, err := initializeSigningKeys(context.Background(), top, keysDir)
	if err != nil || actual != keysDir {
		t.Fatalf("initialize keys: %s, %v", actual, err)
	}
	info, err := os.Stat(keysDir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("key directory permissions: %v, %v", info, err)
	}
	releasePath := filepath.Join(keysDir, "releasekey.pk8")
	releaseKey, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	originalHash := sha256.Sum256(releaseKey)
	for _, name := range releaseKeyNames {
		privatePath := filepath.Join(keysDir, name+".pk8")
		privateDER, err := os.ReadFile(privatePath)
		if err != nil {
			t.Fatal(err)
		}
		privateInfo, err := os.Stat(privatePath)
		if err != nil || privateInfo.Mode().Perm() != 0600 {
			t.Fatalf("private key permissions for %s: %v, %v", name, privateInfo, err)
		}
		privateKey, err := x509.ParsePKCS8PrivateKey(privateDER)
		if err != nil {
			t.Fatalf("parse %s private key: %v", name, err)
		}
		certificatePEM, err := os.ReadFile(filepath.Join(keysDir, name+".x509.pem"))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(certificatePEM)
		if block == nil {
			t.Fatalf("missing certificate PEM for %s", name)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if certificate.PublicKey.(*rsa.PublicKey).N.Cmp(privateKey.(*rsa.PrivateKey).N) != 0 {
			t.Fatalf("certificate and private key differ for %s", name)
		}
	}
	if link, err := os.Readlink(filepath.Join(keysDir, "testkey.pk8")); err != nil || link != "releasekey.pk8" {
		t.Fatalf("testkey alias: %q, %v", link, err)
	}
	if _, err := initializeSigningKeys(context.Background(), top, keysDir); err == nil {
		t.Fatal("existing signing keys were accepted for replacement")
	}
	targetFiles := filepath.Join(t.TempDir(), "target_files.zip")
	writeSigningTargetFiles(t, targetFiles)
	toolsDir := t.TempDir()
	tools := signingTools{
		signTargetFiles: filepath.Join(toolsDir, "sign_target_files_apks"),
		otaFromTarget:   filepath.Join(toolsDir, "ota_from_target_files"),
	}
	writeExecutable(t, tools.signTargetFiles, "previous=; last=; for argument in \"$@\"; do previous=$last; last=$argument; done; cp \"$previous\" \"$last\"")
	writeExecutable(t, tools.otaFromTarget, "cp \"$3\" \"$4\"")
	for range 2 {
		if _, err := signTargetFiles(context.Background(), targetFiles, keysDir, "", t.TempDir(), "product", true, false, tools); err != nil {
			t.Fatalf("reuse signing keys: %v", err)
		}
	}
	reused, err := os.ReadFile(releasePath)
	if err != nil || sha256.Sum256(reused) != originalHash {
		t.Fatalf("release key changed after repeated initialization: %v", err)
	}
}

func TestRunInitializesKeysWithoutLunchOrBuild(t *testing.T) {
	top := t.TempDir()
	if err := os.MkdirAll(filepath.Join(top, "build", "soong"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(top, "build", "soong", "root.bp"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOP", top)
	t.Setenv("TARGET_PRODUCT", "")
	keysDir := filepath.Join(t.TempDir(), "android-certs")
	options, err := ParseOptions([]string{"--init-signing-keys", keysDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "out")); !os.IsNotExist(err) {
		t.Fatalf("key initialization touched build output: %v", err)
	}
}

func TestInitializeSigningKeysRejectsSourceTree(t *testing.T) {
	top := t.TempDir()
	if _, err := initializeSigningKeys(context.Background(), top, filepath.Join(top, "keys")); err == nil {
		t.Fatal("signing keys were created inside source tree")
	}
}

func TestInitializeSigningKeysRejectsBuildOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--init-signing-keys=", "otapackage"},
		{"--init-signing-keys", "keys", "otapackage"},
		{"--init-signing-keys", "keys", "--sign-keys", "keys"},
		{"--init-signing-keys", "keys", "-j18"},
	} {
		if _, err := ParseOptions(args); err == nil {
			t.Fatalf("invalid key initialization accepted: %v", args)
		}
	}
}

func writeSigningTargetFiles(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, content := range map[string]string{
		"META/apkcerts.txt": "name=\"Example.apk\" certificate=\"build/make/target/product/security/platform\" private_key=\"build/make/target/product/security/platform\"\n",
		"META/apexkeys.txt": "name=\"com.android.example.apex\" public_key=\"source/example.avbpubkey\" private_key=\"source/example.pem\" container_certificate=\"source/example.x509.pem\" container_private_key=\"source/example.pk8\" partition=\"system\"\n" +
			"name=\"com.google.presigned.apex\" public_key=\"PRESIGNED\" private_key=\"PRESIGNED\" container_certificate=\"PRESIGNED\" container_private_key=\"PRESIGNED\" partition=\"product\"\n",
		"META/misc_info.txt": "default_system_dev_certificate=build/make/target/product/security/testkey\n",
	} {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedApexSigningKeysAreReused(t *testing.T) {
	top := t.TempDir()
	keysDir := filepath.Join(t.TempDir(), "android-certs")
	if _, err := initializeSigningKeys(context.Background(), top, keysDir); err != nil {
		t.Fatal(err)
	}
	targetFiles := filepath.Join(t.TempDir(), "target_files.zip")
	writeSigningTargetFiles(t, targetFiles)
	var firstHash [sha256.Size]byte
	for attempt := range 2 {
		containers, payloads, err := managedApexSigningMappings(context.Background(), targetFiles, keysDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(containers) != 1 || len(payloads) != 1 || containers["com.google.presigned.apex"] != "" {
			t.Fatalf("unexpected APEX signing mappings: %v, %v", containers, payloads)
		}
		base := containers["com.android.example.apex"]
		if payloads["com.android.example.apex"] != base+".pem" {
			t.Fatalf("APEX payload key mismatch: %v", payloads)
		}
		privateDER, err := os.ReadFile(base + ".pk8")
		if err != nil {
			t.Fatal(err)
		}
		key, err := x509.ParsePKCS8PrivateKey(privateDER)
		if err != nil || key.(*rsa.PrivateKey).N.BitLen() != 4096 {
			t.Fatalf("APEX payload key is not RSA4096: %v", err)
		}
		payloadPEM, err := os.ReadFile(base + ".pem")
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(payloadPEM)
		if block == nil || block.Type != "PRIVATE KEY" {
			t.Fatalf("invalid APEX payload PEM: %v", block)
		}
		hash := sha256.Sum256(privateDER)
		if attempt == 0 {
			firstHash = hash
		} else if hash != firstHash {
			t.Fatal("APEX signing key changed between releases")
		}
	}
	toolDir := t.TempDir()
	argsFile := filepath.Join(toolDir, "sign-args")
	t.Setenv("SIGN_ARGS_FILE", argsFile)
	tools := signingTools{
		signTargetFiles: filepath.Join(toolDir, "sign_target_files_apks"),
		otaFromTarget:   filepath.Join(toolDir, "ota_from_target_files"),
	}
	writeExecutable(t, tools.signTargetFiles, "printf '%s\\n' \"$@\" > \"$SIGN_ARGS_FILE\"\nprevious=; last=; for argument in \"$@\"; do previous=$last; last=$argument; done; cp \"$previous\" \"$last\"")
	writeExecutable(t, tools.otaFromTarget, "cp \"$3\" \"$4\"")
	if _, err := signTargetFiles(context.Background(), targetFiles, keysDir, "", t.TempDir(), "product", true, false, tools); err != nil {
		t.Fatalf("sign with managed APEX keys: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "com.android.example.apex="+filepath.Join(keysDir, "apex", "com.android.example.apex", "key")) ||
		!strings.Contains(string(args), "com.android.example.apex="+filepath.Join(keysDir, "apex", "com.android.example.apex", "key.pem")) ||
		strings.Contains(string(args), "com.google.presigned.apex=") {
		t.Fatalf("unexpected APEX signing flags: %s", args)
	}
	manualDir := t.TempDir()
	writeSigningKeys(t, manualDir)
	containers, payloads, err := managedApexSigningMappings(context.Background(), targetFiles, manualDir)
	if err != nil || len(containers) != 0 || len(payloads) != 0 {
		t.Fatalf("manual signing keys were treated as managed: %v, %v, %v", containers, payloads, err)
	}
	if _, err := os.Stat(filepath.Join(manualDir, "apex")); !os.IsNotExist(err) {
		t.Fatalf("manual signing directory was modified: %v", err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

func writeSigningKeys(t *testing.T, directory string) {
	t.Helper()
	for _, name := range []string{"releasekey", "platform", "shared", "media"} {
		for _, suffix := range []string{".pk8", ".x509.pem"} {
			if err := os.WriteFile(filepath.Join(directory, name+suffix), []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSigningBuildOptions(t *testing.T) {
	options, err := ParseOptions([]string{"--sign-keys", "keys", "-j18", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.FullBuild || options.PackageMode != packageModeOTA ||
		!strings.Contains(strings.Join(options.BuildArgs, " "), "target-files-package otatools") ||
		strings.Contains(strings.Join(options.BuildArgs, " "), "otapackage") {
		t.Fatalf("unexpected signing build options: %+v", options)
	}
	options, err = ParseOptions([]string{"--sign-keys", "keys", "--sign-path", "Sign.Out", "-j18", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	if options.SignPath != "Sign.Out" {
		t.Fatalf("sign path = %q", options.SignPath)
	}
	for _, args := range [][]string{
		{"--sign-keys", "keys", "uwu-f"},
		{"--sign-keys", "keys", "uwu", "uwu-f"},
	} {
		options, err = ParseOptions(args)
		if err != nil {
			t.Fatalf("signed release target %v failed: %v", args, err)
		}
		if !reflect.DeepEqual(options.Targets, []string{"target-files-package", "otatools"}) ||
			strings.Contains(strings.Join(options.BuildArgs, " "), "updatepackage") {
			t.Fatalf("signed release target %v produced unexpected options: %+v", args, options)
		}
	}
	for _, args := range [][]string{
		{"--sign-keys", "keys", "--trust-output", "otapackage"},
		{"--sign-keys", "keys", "updatepackage"},
		{"--sign-check"},
		{"--sign-keys", "keys", "--sign-check", "otapackage"},
		{"--sign-path", "Sign.Out", "otapackage"},
	} {
		if _, err := ParseOptions(args); err == nil {
			t.Fatalf("unsafe signing options accepted: %v", args)
		}
	}
}

func TestApplyVendorPrivateKeySigningPolicy(t *testing.T) {
	top := t.TempDir()
	privateKeysDir := filepath.Join(top, "vendor", "uwu-priv", "keys")
	if err := os.MkdirAll(privateKeysDir, 0755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name            string
		options         Options
		wantBuildArgs   []string
		wantTargets     []string
		wantSignKeys    string
		wantSkipOTASign bool
		wantError       bool
	}{
		{
			name:          "OTA uses build system signing",
			options:       Options{SignKeys: "external-keys", BuildArgs: []string{"-j18", "target-files-package", "otatools"}, Targets: []string{"target-files-package", "otatools"}, PackageMode: packageModeOTA},
			wantBuildArgs: []string{"-j18", "otapackage"},
			wantTargets:   []string{"otapackage"},
		},
		{
			name:            "combined keeps fastboot signing only",
			options:         Options{SignKeys: "external-keys", BuildArgs: []string{"target-files-package", "otatools"}, Targets: []string{"target-files-package", "otatools"}, PackageMode: packageModeBoth},
			wantBuildArgs:   []string{"target-files-package", "otatools", "otapackage"},
			wantTargets:     []string{"target-files-package", "otatools", "otapackage"},
			wantSignKeys:    "external-keys",
			wantSkipOTASign: true,
		},
		{
			name:          "fastboot-only signing is unchanged",
			options:       Options{SignKeys: "external-keys", BuildArgs: []string{"target-files-package", "otatools"}, Targets: []string{"target-files-package", "otatools"}, PackageMode: packageModeFastboot},
			wantBuildArgs: []string{"target-files-package", "otatools"},
			wantTargets:   []string{"target-files-package", "otatools"},
			wantSignKeys:  "external-keys",
		},
		{
			name:      "signing check is rejected",
			options:   Options{SignKeys: "external-keys", SignCheck: true},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := applyVendorPrivateKeySigningPolicy(top, test.options)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %t", err, test.wantError)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(options.BuildArgs, test.wantBuildArgs) ||
				!reflect.DeepEqual(options.Targets, test.wantTargets) || options.SignKeys != test.wantSignKeys ||
				options.SkipOTASigning != test.wantSkipOTASign {
				t.Fatalf("unexpected policy result: options=%+v", options)
			}
		})
	}
}

func TestSignTargetFilesCanSkipOTA(t *testing.T) {
	directory := t.TempDir()
	keys := filepath.Join(directory, "keys")
	if err := os.Mkdir(keys, 0700); err != nil {
		t.Fatal(err)
	}
	writeSigningKeys(t, keys)
	targetFiles := filepath.Join(directory, "target_files.zip")
	writeSigningTargetFiles(t, targetFiles)
	tools := signingTools{signTargetFiles: filepath.Join(directory, "sign_target_files_apks")}
	writeExecutable(t, tools.signTargetFiles, "previous=; last=; for argument in \"$@\"; do previous=$last; last=$argument; done; cp \"$previous\" \"$last\"")
	result, err := signTargetFiles(context.Background(), targetFiles, keys, "", t.TempDir(), "product", false, false, tools)
	if err != nil {
		t.Fatal(err)
	}
	if result.SignedOTA != "" || result.Checksum != "" {
		t.Fatalf("OTA was generated for a fastboot-only signing run: %+v", result)
	}
	if info, err := os.Stat(result.SignedTargetFiles); err != nil || info.Size() == 0 {
		t.Fatalf("missing signed target-files: %v", err)
	}
}

func TestBuildSignedFastbootPackage(t *testing.T) {
	directory := t.TempDir()
	targetFiles := filepath.Join(directory, "signed-target_files.zip")
	if err := os.WriteFile(targetFiles, []byte("signed"), 0644); err != nil {
		t.Fatal(err)
	}
	tools := signingTools{
		imgFromTarget:    filepath.Join(directory, "img_from_target_files"),
		buildSuperImage:  filepath.Join(directory, "build_super_image"),
		workingDirectory: directory,
	}
	writeExecutable(t, tools.imgFromTarget, "previous=; last=; for argument in \"$@\"; do previous=$last; last=$argument; done; cp \"$previous\" \"$last\"")
	writeExecutable(t, tools.buildSuperImage, "exit 0")
	output, err := buildSignedFastbootPackage(context.Background(), targetFiles, directory, "uwu_fuxi", tools)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 || !strings.Contains(filepath.Base(output), "-img-signed-") {
		t.Fatalf("missing signed fastboot package %q: %v", output, err)
	}
}

func TestSignTargetFilesUsesIsolatedOutput(t *testing.T) {
	directory := t.TempDir()
	keys := filepath.Join(directory, "keys")
	if err := os.Mkdir(keys, 0700); err != nil {
		t.Fatal(err)
	}
	writeSigningKeys(t, keys)
	targetFiles := filepath.Join(directory, "target_files.zip")
	writeSigningTargetFiles(t, targetFiles)
	tools := signingTools{
		signTargetFiles: filepath.Join(directory, "sign_target_files_apks"),
		otaFromTarget:   filepath.Join(directory, "ota_from_target_files"),
	}
	writeExecutable(t, tools.signTargetFiles, "cp \"$4\" \"$5\"")
	writeExecutable(t, tools.otaFromTarget, "cp \"$3\" \"$4\"")
	output := filepath.Join(directory, "release", "product", "checks", "run")
	result, err := signTargetFiles(context.Background(), targetFiles, keys, "", output, "product", true, true, tools)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Check || !strings.Contains(result.SignedOTA, filepath.Join("checks", "run")) {
		t.Fatalf("signing result escaped isolated output: %+v", result)
	}
	for _, artifact := range []string{result.SignedTargetFiles, result.SignedOTA, result.Checksum} {
		if info, err := os.Stat(artifact); err != nil || info.Size() == 0 {
			t.Fatalf("missing signing artifact %s: %v", artifact, err)
		}
	}
}

func TestSigningCommandUsesSourceRoot(t *testing.T) {
	directory := t.TempDir()
	workingDirectory := filepath.Join(directory, "source")
	if err := os.Mkdir(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(directory, "tool")
	output := filepath.Join(directory, "working-directory")
	writeExecutable(t, tool, "pwd > \"$1\"")
	if err := signingCommand(context.Background(), tool, workingDirectory, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), workingDirectory; got != want {
		t.Fatalf("signing command directory = %q, want %q", got, want)
	}
}

func TestFindTargetFilesRejectsAmbiguousInput(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "obj", "PACKAGING", "target_files_intermediates")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first-target_files.zip", "second-target_files.zip"} {
		if err := os.WriteFile(filepath.Join(path, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := findTargetFiles(directory); err == nil {
		t.Fatal("ambiguous target-files input was accepted")
	}
}

func TestResolveSigningOutputDirectory(t *testing.T) {
	top := t.TempDir()
	outDir := filepath.Join(top, "out")
	explicit := filepath.Join(top, "custom-sign")
	got, err := resolveSigningOutputDirectory(top, outDir, explicit, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("explicit signing directory = %q, want %q", got, explicit)
	}
	if info, err := os.Stat(explicit); err != nil || !info.IsDir() {
		t.Fatalf("explicit signing directory was not created: %v", err)
	}

	if err := os.Mkdir(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	got, err = resolveSigningOutputDirectory(top, outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != outDir {
		t.Fatalf("empty out signing directory = %q, want %q", got, outDir)
	}

	if err := os.WriteFile(filepath.Join(outDir, "marker"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err = resolveSigningOutputDirectory(top, outDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(top, "Sign.Out")
	if got != want {
		t.Fatalf("occupied out signing directory = %q, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("Sign.Out was not created: %v", err)
	}

	got, err = resolveSigningOutputDirectory(top, outDir, explicit, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, filepath.Join(explicit, "checks")) {
		t.Fatalf("signing check directory = %q, want it under %s", got, explicit)
	}
}
