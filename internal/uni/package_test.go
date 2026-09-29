// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeReleaseBuildProp(t *testing.T, productOut string) {
	t.Helper()
	directory := filepath.Join(productOut, "product", "etc")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, time.September, 29, 12, 19, 57, 0, time.UTC).Unix()
	data := strings.Join([]string{
		"ro.product.build.date.utc=" + strconv.FormatInt(timestamp, 10),
		"ro.build.tags=test-keys",
		"ro.uwu.release=17.0.100",
		"ro.uwu.build.version=ignored",
		"ro.uwu.releasetype=UNOFFICIAL",
		"ro.uwu.device=fuxi",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(directory, "build.prop"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPublishReleasePackages(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		suffix string
		ending string
	}{
		{name: "ota", mode: packageModeOTA, suffix: "-ota.zip", ending: "-unsigned.zip"},
		{name: "fastboot", mode: packageModeFastboot, suffix: "-img.zip", ending: "-unsigned-fastboot.zip"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			productOut := t.TempDir()
			writeReleaseBuildProp(t, productOut)
			state := State{ProductOut: productOut, TargetProduct: "uwu_fuxi"}
			source := filepath.Join(productOut, state.TargetProduct+test.suffix)
			if err := os.WriteFile(source, []byte("package"), 0644); err != nil {
				t.Fatal(err)
			}
			published, err := publishReleasePackage(state, test.mode, "", "")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(productOut, "uwuAOSP-17.0.100-20260929-UNOFFICIAL-fuxi"+test.ending)
			if published.path != want {
				t.Fatalf("package path = %q, want %q", published.path, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Fatalf("source package was not renamed: %v", err)
			}
		})
	}
}

func TestPublishSignedPackageRenamesChecksum(t *testing.T) {
	productOut := t.TempDir()
	writeReleaseBuildProp(t, productOut)
	output := t.TempDir()
	source := filepath.Join(output, "uwu_fuxi-ota-signed.zip")
	checksum := source + ".sha256"
	if err := os.WriteFile(source, []byte("signed package"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksum, []byte("deadbeef  uwu_fuxi-ota-signed.zip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	published, err := publishReleasePackage(
		State{ProductOut: productOut, TargetProduct: "uwu_fuxi"},
		packageModeOTA, source, checksum)
	if err != nil {
		t.Fatal(err)
	}
	wantName := "uwuAOSP-17.0.100-20260929-UNOFFICIAL-fuxi-signed.zip"
	if filepath.Base(published.path) != wantName || published.checksum != published.path+".sha256" {
		t.Fatalf("unexpected signed output: %+v", published)
	}
	data, err := os.ReadFile(published.checksum)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "deadbeef  "+wantName+"\n"; got != want {
		t.Fatalf("checksum = %q, want %q", got, want)
	}
}

func TestPublishBothReleasePackages(t *testing.T) {
	productOut := t.TempDir()
	writeReleaseBuildProp(t, productOut)
	state := State{ProductOut: productOut, TargetProduct: "uwu_fuxi"}
	for _, suffix := range []string{"-ota.zip", "-img.zip"} {
		if err := os.WriteFile(filepath.Join(productOut, state.TargetProduct+suffix), []byte("package"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	packages, err := publishReleasePackages(state, Options{PackageMode: packageModeBoth}, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || !strings.HasSuffix(packages[0].path, "-unsigned.zip") ||
		!strings.HasSuffix(packages[1].path, "-unsigned-fastboot.zip") {
		t.Fatalf("unexpected combined packages: %+v", packages)
	}
}
