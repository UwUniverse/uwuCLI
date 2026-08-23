// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"reflect"
	"testing"
)

func TestParseOptionsFullBuild(t *testing.T) {
	options, err := ParseOptions([]string{
		"-j", "12", "-k3", "--uni-batch-size=550",
		"-l", "18.5", "--debug", "-dev", "--uni-assume-existing", "FOO=bar", "otapackage", "dist",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.FullBuild || !options.Dist || options.MaxJobs != 12 || options.KeepGoing != 3 {
		t.Fatalf("unexpected options: %+v", options)
	}
	if !options.Debug || !options.Dev || !options.TrustOutput || !options.AssumeExisting {
		t.Fatal("debug or developer mode was not enabled")
	}
	if options.BatchSize != 550 {
		t.Fatalf("unexpected limits: %+v", options)
	}
	if !options.LoadSet || options.LoadAverage != 18.5 {
		t.Fatalf("unexpected load average: %+v", options)
	}
	if !reflect.DeepEqual(options.KeyValues, []string{"FOO=bar"}) {
		t.Fatalf("unexpected key values: %v", options.KeyValues)
	}
	if got := prepareArgs(options, 8); !reflect.DeepEqual(got,
		[]string{"-j8", "-k3", "FOO=bar", "otapackage", "dist"}) {
		t.Fatalf("unexpected prepare args: %v", got)
	}
}

func TestParseOptionsModuleBuild(t *testing.T) {
	options, err := ParseOptions([]string{"SystemUI"})
	if err != nil {
		t.Fatal(err)
	}
	if options.FullBuild {
		t.Fatal("module build was classified as a full build")
	}
	if !reflect.DeepEqual(options.Targets, []string{"SystemUI"}) {
		t.Fatalf("unexpected targets: %v", options.Targets)
	}
}

func TestParseOptionsAutomaticDeveloperMode(t *testing.T) {
	options, err := ParseOptions([]string{"-dev_1", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.DevAuto || options.Dev {
		t.Fatalf("unexpected developer options: %+v", options)
	}
	if !options.DevAutoSet {
		t.Fatal("automatic developer setting was not marked explicit")
	}
	if !reflect.DeepEqual(options.BuildArgs, []string{"otapackage"}) {
		t.Fatalf("automatic developer option leaked into build arguments: %v", options.BuildArgs)
	}
	if _, err := ParseOptions([]string{"-dev", "-dev_1"}); err == nil {
		t.Fatal("conflicting developer options were accepted")
	}
	disabled, err := ParseOptions([]string{"-dev_0", "otapackage"})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DevAuto || !disabled.DevAutoSet {
		t.Fatalf("unexpected disabled developer options: %+v", disabled)
	}
}

func TestParseOptionsSnodIsSinglePhase(t *testing.T) {
	options, err := ParseOptions([]string{"snod"})
	if err != nil {
		t.Fatal(err)
	}
	if options.FullBuild || !reflect.DeepEqual(options.Targets, []string{"snod"}) {
		t.Fatalf("unexpected snod options: %+v", options)
	}
}

func TestParseOptionsAcceptsKeepGoingZero(t *testing.T) {
	for _, args := range [][]string{{"-k0", "SystemUI"}, {"-k", "0", "SystemUI"}} {
		options, err := ParseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if options.KeepGoing != 0 {
			t.Fatalf("unexpected keep-going value for %v: %d", args, options.KeepGoing)
		}
	}
}

func TestParseOptionsAcceptsJobsWithoutValue(t *testing.T) {
	options, err := ParseOptions([]string{"-j", "SystemUI"})
	if err != nil {
		t.Fatal(err)
	}
	if options.MaxJobs != 0 || !reflect.DeepEqual(options.Targets, []string{"SystemUI"}) {
		t.Fatalf("unexpected options: %+v", options)
	}
}

func TestParseOptionsDefaultAndDistAreFullBuilds(t *testing.T) {
	for _, args := range [][]string{nil, {"dist"}} {
		options, err := ParseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if !options.FullBuild {
			t.Fatalf("%v was not classified as a full build", args)
		}
	}
}

func TestParseOptionsRejectsInvalidBatch(t *testing.T) {
	if _, err := ParseOptions([]string{"--uni-batch-size=100"}); err == nil {
		t.Fatal("invalid batch size was accepted")
	}
}

func TestParseOptionsLoadAverage(t *testing.T) {
	for _, args := range [][]string{{"-l12", "SystemUI"}, {"--load-average=12", "SystemUI"}} {
		options, err := ParseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if !options.LoadSet || options.LoadAverage != 12 {
			t.Fatalf("unexpected load average for %v: %+v", args, options)
		}
	}
	if _, err := ParseOptions([]string{"-l0", "SystemUI"}); err == nil {
		t.Fatal("invalid load average was accepted")
	}
}

func TestAutomaticNinjaLoadLimit(t *testing.T) {
	for _, test := range []struct {
		jobs int
		cpus int
		want int
	}{{18, 18, 27}, {14, 18, 21}, {32, 18, 27}, {1, 18, 3}} {
		if got := automaticNinjaLoadLimit(test.jobs, test.cpus); got != test.want {
			t.Fatalf("jobs=%d cpus=%d: got %d, want %d", test.jobs, test.cpus, got, test.want)
		}
	}
}

func TestPhaseArgs(t *testing.T) {
	options := Options{
		KeepGoing:    4,
		KeyValues:    []string{"FOO=bar"},
		LoadAverage:  10,
		LoadSet:      true,
		ShowCommands: true,
	}
	want := []string{"-j6", "-k4", "FOO=bar", "NINJA_ARGS=-l 10 -v", "dist", "droid"}
	if got := phaseArgs(options, []string{"droid"}, 6, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPhaseArgsPreservesNinjaArgsWithShowCommands(t *testing.T) {
	options := Options{
		KeepGoing:    1,
		KeyValues:    []string{"NINJA_ARGS=-d explain"},
		LoadAverage:  10,
		LoadSet:      true,
		ShowCommands: true,
	}
	want := []string{"-j6", "NINJA_ARGS=-d explain -l 10 -v", "droid"}
	if got := phaseArgs(options, []string{"droid"}, 6, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPhaseArgsPreservesExplicitNinjaLoadLimit(t *testing.T) {
	options := Options{
		KeepGoing: 1,
		KeyValues: []string{"NINJA_ARGS=-d explain -l 8"},
	}
	want := []string{"-j6", "NINJA_ARGS=-d explain -l 8", "droid"}
	if got := phaseArgs(options, []string{"droid"}, 6, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPhaseArgsEnablesAssumeExisting(t *testing.T) {
	options := Options{
		AssumeExisting: true,
		KeepGoing:      1,
		KeyValues:      []string{"NINJA_ARGS=-d explain"},
		LoadAverage:    9,
		LoadSet:        true,
	}
	want := []string{"-j6", "NINJA_ARGS=-d explain -l 9 -d assumeexisting", "droid"}
	if got := phaseArgs(options, []string{"droid"}, 6, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
