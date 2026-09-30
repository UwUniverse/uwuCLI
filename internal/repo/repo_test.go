// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package repo

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadProjectsUsesDefaultRemoteAndNamePath(t *testing.T) {
	top := t.TempDir()
	bin := filepath.Join(top, "repo")
	script := "#!/bin/sh\nprintf '%s' '{\"default\":{\"remote\":\"aosp\"},\"project\":[{\"name\":\"platform/frameworks/base\"},{\"name\":\"uwuCLI\",\"remote\":\"UwUniverse\"},{\"name\":\"vendor/uwu\",\"remote\":\"uwuAOSP\"}]}'\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	projects, err := loadProjects(top, bin)
	if err != nil {
		t.Fatal(err)
	}
	want := []project{
		{Path: "platform/frameworks/base", Remote: "aosp"},
		{Path: "uwuCLI", Remote: "UwUniverse"},
		{Path: "vendor/uwu", Remote: "uwuAOSP"},
	}
	if !reflect.DeepEqual(projects, want) {
		t.Fatalf("projects = %#v, want %#v", projects, want)
	}
}

func TestFindSourceRootFromNestedDirectory(t *testing.T) {
	top := t.TempDir()
	if err := os.Mkdir(filepath.Join(top, ".repo"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(top, "uwuCLI", "internal", "repo")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := findSourceRootFrom(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != top {
		t.Fatalf("source root = %q, want %q", got, top)
	}
}

func TestSelectProjectsExcludesUwuaosp(t *testing.T) {
	projects := []project{
		{Path: "build/make", Remote: "uwuAOSP"},
		{Path: "frameworks/base", Remote: "uwuAOSP"},
		{Path: "packages/apps/Settings", Remote: "aosp"},
		{Path: "uwuCLI", Remote: "UwUniverse"},
	}
	selected, syncArgs, err := selectProjects(t.TempDir(), projects, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []string{"packages/apps/Settings", "uwuCLI"}) {
		t.Fatalf("selected = %#v", selected)
	}
	if len(syncArgs) != 0 {
		t.Fatalf("sync args = %#v", syncArgs)
	}
}

func TestSelectProjectsSupportsSelectorsAndRepoOptions(t *testing.T) {
	projects := []project{
		{Path: "frameworks/base", Remote: "aosp"},
		{Path: "frameworks/native", Remote: "aosp"},
		{Path: "packages/apps/Settings", Remote: "aosp"},
	}
	selected, syncArgs, err := selectProjects(t.TempDir(), projects, []string{"frameworks", "--jobs", "4"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []string{"frameworks/base", "frameworks/native"}) {
		t.Fatalf("selected = %#v", selected)
	}
	if !reflect.DeepEqual(syncArgs, []string{"--jobs", "4"}) {
		t.Fatalf("sync args = %#v", syncArgs)
	}
}

func TestSelectProjectsSupportsShellPatternsAcrossDirectories(t *testing.T) {
	projects := []project{
		{Path: "packages/apps/Settings", Remote: "aosp"},
		{Path: "packages/modules/Connectivity", Remote: "aosp"},
		{Path: "system/core", Remote: "aosp"},
	}
	selected, _, err := selectProjects(t.TempDir(), projects, []string{"packages/*"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []string{"packages/apps/Settings", "packages/modules/Connectivity"}) {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectProjectsCanIncludeUwuaosp(t *testing.T) {
	projects := []project{
		{Path: "build/make", Remote: "uwuAOSP"},
		{Path: "uwuCLI", Remote: "UwUniverse"},
	}
	selected, _, err := selectProjects(t.TempDir(), projects, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []string{"build/make", "uwuCLI"}) {
		t.Fatalf("selected = %#v", selected)
	}
}

func TestSelectProjectsRejectsExcludedOnlySelector(t *testing.T) {
	projects := []project{{Path: "vendor/uwu", Remote: "uwuAOSP"}}
	_, _, err := selectProjects(t.TempDir(), projects, []string{"vendor/uwu"}, false)
	if err == nil || !strings.Contains(err.Error(), "vendor/uwu") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildSyncArgsPreventsManifestProjectDrift(t *testing.T) {
	got := buildSyncArgs([]string{"--jobs", "4"}, []string{"frameworks/base", "uwuCLI"})
	want := []string{"sync", "--no-manifest-update", "--jobs", "4", "frameworks/base", "uwuCLI"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sync args = %#v, want %#v", got, want)
	}
}

func TestBuildSyncArgsPreservesExplicitManifestOption(t *testing.T) {
	got := buildSyncArgs([]string{"--nmu", "frameworks/base"}, []string{"uwuCLI"})
	want := []string{"sync", "--nmu", "frameworks/base", "uwuCLI"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sync args = %#v, want %#v", got, want)
	}
}
