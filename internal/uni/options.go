// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultBatchSize = 500
	minimumBatchSize = 350
	maximumBatchSize = 4096
	gibibyte         = 1024 * 1024 * 1024
)

type Options struct {
	BuildArgs      []string
	KeyValues      []string
	KeepGoing      int
	MaxJobs        int
	LoadAverage    float64
	BatchSize      int
	LoadSet        bool
	Static         bool
	Debug          bool
	Dev            bool
	DevAuto        bool
	DevAutoSet     bool
	Plan           bool
	Help           bool
	FullBuild      bool
	Dist           bool
	ShowCommands   bool
	TrustOutput    bool
	AssumeExisting bool
	ForceReuse     bool
	Targets        []string
}

func Usage() string {
	return `Usage: uni [m options] [targets...]

uni prepares Soong and Kati once, then schedules Ninja in memory-aware segments.
Normal m behavior is unchanged.

uni options:
  --uni-batch-size=N       Initial package targets per segment (350-4096; default 500)
  --uni-static             Disable dynamic batch and job adjustment
  --uni-plan               Prepare the graph and print the schedule without running Ninja
  --uni-trust-output       Keep all recovered Ninja outputs without validation
  --uni-assume-existing    Trust existing outputs when Ninja log entries are missing
  --uni-force-reuse        Reuse the prepared graph without source freshness checks
  --debug                   Write a concise build debug report into OUT_DIR
  -dev                      Force a complete R8 index scan from Ninja files
  -dev_1                    Persist automatic R8 index refresh
  -dev_0                    Disable automatic R8 index refresh
  -h, --help               Show this help

Examples:
  uni -j8 otapackage
  uni -j8 -dev otapackage
  uni -j8 -dev_1 otapackage
  uni SystemUI
  uni dist
`
}

func parsePositiveInt(value, name string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func parseNonNegativeInt(value, name string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func parsePositiveFloat(value, name string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive number", name)
	}
	return n, nil
}

func customValue(args []string, index *int, name string) (string, bool, error) {
	arg := args[*index]
	prefix := name + "="
	if strings.HasPrefix(arg, prefix) {
		return strings.TrimPrefix(arg, prefix), true, nil
	}
	if arg != name {
		return "", false, nil
	}
	if *index+1 >= len(args) {
		return "", true, fmt.Errorf("%s requires a value", name)
	}
	*index = *index + 1
	return args[*index], true, nil
}

func ParseOptions(args []string) (Options, error) {
	options := Options{
		BatchSize: defaultBatchSize,
		KeepGoing: 1,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			options.Help = true
			continue
		case "--uni-static":
			options.Static = true
			continue
		case "--uni-plan":
			options.Plan = true
			continue
		case "--uni-trust-output":
			options.TrustOutput = true
			continue
		case "--uni-assume-existing":
			options.AssumeExisting = true
			options.TrustOutput = true
			continue
		case "--uni-force-reuse":
			options.ForceReuse = true
			continue
		case "--debug":
			options.Debug = true
			continue
		case "-dev":
			options.Dev = true
			continue
		case "-dev_1":
			options.DevAuto = true
			options.DevAutoSet = true
			continue
		case "-dev_0":
			options.DevAuto = false
			options.DevAutoSet = true
			continue
		case "showcommands":
			options.ShowCommands = true
			continue
		}
		if value, matched, err := customValue(args, &i, "--uni-batch-size"); matched {
			if err != nil {
				return Options{}, err
			}
			batchSize, err := parsePositiveInt(value, "batch size")
			if err != nil {
				return Options{}, err
			}
			if batchSize < minimumBatchSize || batchSize > maximumBatchSize {
				return Options{}, fmt.Errorf("batch size must be between %d and %d", minimumBatchSize, maximumBatchSize)
			}
			options.BatchSize = batchSize
			continue
		}
		if value, matched, err := customValue(args, &i, "--load-average"); matched {
			if err != nil {
				return Options{}, err
			}
			loadAverage, err := parsePositiveFloat(value, "--load-average")
			if err != nil {
				return Options{}, err
			}
			options.LoadAverage = loadAverage
			options.LoadSet = true
			continue
		}
		if strings.HasPrefix(arg, "-j") {
			value := strings.TrimPrefix(arg, "-j")
			if value == "" {
				if i+1 < len(args) {
					if _, err := strconv.Atoi(args[i+1]); err == nil {
						i++
						value = args[i]
					}
				}
				if value == "" {
					options.BuildArgs = append(options.BuildArgs, "-j")
					continue
				}
			}
			jobs, err := parsePositiveInt(value, "-j")
			if err != nil {
				return Options{}, err
			}
			options.MaxJobs = jobs
			options.BuildArgs = append(options.BuildArgs, "-j"+strconv.Itoa(jobs))
			continue
		}
		if strings.HasPrefix(arg, "-k") {
			value := strings.TrimPrefix(arg, "-k")
			if value == "" && i+1 < len(args) {
				if _, err := strconv.Atoi(args[i+1]); err == nil {
					i++
					value = args[i]
				}
			}
			if value == "" {
				options.KeepGoing = 0
				options.BuildArgs = append(options.BuildArgs, "-k")
				continue
			}
			keepGoing, err := parseNonNegativeInt(value, "-k")
			if err != nil {
				return Options{}, err
			}
			options.KeepGoing = keepGoing
			options.BuildArgs = append(options.BuildArgs, "-k"+strconv.Itoa(keepGoing))
			continue
		}
		if strings.HasPrefix(arg, "-l") {
			value := strings.TrimPrefix(arg, "-l")
			if value == "" && i+1 < len(args) {
				i++
				value = args[i]
			}
			loadAverage, err := parsePositiveFloat(value, "-l")
			if err != nil {
				return Options{}, err
			}
			options.LoadAverage = loadAverage
			options.LoadSet = true
			continue
		}

		options.BuildArgs = append(options.BuildArgs, arg)
		if strings.ContainsRune(arg, '=') && !strings.HasPrefix(arg, "-") {
			options.KeyValues = append(options.KeyValues, arg)
			continue
		}
		if arg == "dist" {
			options.Dist = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		options.Targets = append(options.Targets, arg)
	}

	if len(options.Targets) == 0 {
		options.FullBuild = true
	} else {
		for _, target := range options.Targets {
			switch target {
			case "droid", "droidcore", "otapackage":
				options.FullBuild = true
			}
		}
	}
	if options.Dev && options.DevAutoSet && options.DevAuto {
		return Options{}, fmt.Errorf("-dev and -dev_1 cannot be used together")
	}
	return options, nil
}

func automaticNinjaLoadLimit(jobs, cpus int) int {
	if cpus < 1 {
		cpus = 1
	}
	if jobs < 1 {
		jobs = cpus
	}
	active := min(jobs, cpus)
	return max(active+2, int(math.Ceil(float64(active)*1.5)))
}

func ninjaArgsHaveLoadLimit(value string) bool {
	fields := strings.Fields(value)
	for _, field := range fields {
		if field == "-l" || field == "--load-average" ||
			(strings.HasPrefix(field, "-l") && len(field) > 2) ||
			strings.HasPrefix(field, "--load-average=") {
			return true
		}
	}
	return false
}

func keyValue(keyValues []string, name string) (string, int, bool) {
	prefix := name + "="
	for index, value := range keyValues {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix), index, true
		}
	}
	return "", -1, false
}

func appendNinjaArgument(keyValues []string, argument string) []string {
	value, index, found := keyValue(keyValues, "NINJA_ARGS")
	if !found {
		value = os.Getenv("NINJA_ARGS")
	}
	value = strings.TrimSpace(value + " " + argument)
	entry := "NINJA_ARGS=" + value
	if found {
		keyValues[index] = entry
		return keyValues
	}
	return append(keyValues, entry)
}

func addNinjaLoadLimit(options Options, keyValues []string, jobs int) []string {
	ninjaArgs, _, found := keyValue(keyValues, "NINJA_ARGS")
	if !found {
		ninjaArgs = os.Getenv("NINJA_ARGS")
	}
	extraArgs, _, found := keyValue(keyValues, "NINJA_EXTRA_ARGS")
	if !found {
		extraArgs = os.Getenv("NINJA_EXTRA_ARGS")
	}
	if ninjaArgsHaveLoadLimit(ninjaArgs) || ninjaArgsHaveLoadLimit(extraArgs) {
		return keyValues
	}
	loadAverage := options.LoadAverage
	if !options.LoadSet {
		loadAverage = float64(automaticNinjaLoadLimit(jobs, runtime.NumCPU()))
	}
	argument := "-l " + strconv.FormatFloat(loadAverage, 'f', -1, 64)
	return appendNinjaArgument(keyValues, argument)
}

func prepareArgs(options Options, jobs int) []string {
	args := make([]string, 0, len(options.BuildArgs)+1)
	for _, arg := range options.BuildArgs {
		if !strings.HasPrefix(arg, "-j") {
			args = append(args, arg)
		}
	}
	return append([]string{"-j" + strconv.Itoa(jobs)}, args...)
}

func phaseArgs(options Options, targets []string, jobs int, dist bool) []string {
	args := []string{"-j" + strconv.Itoa(jobs)}
	if options.KeepGoing == 0 {
		args = append(args, "-k")
	} else if options.KeepGoing != 1 {
		args = append(args, "-k"+strconv.Itoa(options.KeepGoing))
	}
	keyValues := append([]string(nil), options.KeyValues...)
	keyValues = addNinjaLoadLimit(options, keyValues, jobs)
	if options.AssumeExisting {
		keyValues = appendNinjaArgument(keyValues, "-d assumeexisting")
	}
	if options.ShowCommands {
		keyValues = appendNinjaArgument(keyValues, "-v")
	}
	args = append(args, keyValues...)
	if dist {
		args = append(args, "dist")
	}
	return append(args, targets...)
}
