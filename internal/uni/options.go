// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultBatchSize          = maximumBatchSize
	minimumBatchSize          = 350
	maximumBatchSize          = 4096
	automaticMaximumBatchSize = maximumBatchSize
	gibibyte                  = 1024 * 1024 * 1024
)

type Options struct {
	RawArgs        []string
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
	CleanLogs      bool
	NoTUI          bool
	Targets        []string
}

func Usage() string {
	locale := ""
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANGUAGE", "LANG"} {
		if locale = strings.TrimSpace(os.Getenv(name)); locale != "" {
			break
		}
	}
	if strings.HasPrefix(strings.ToLower(locale), "zh") {
		return usageChinese
	}
	return usageEnglish
}

const usageEnglish = `Usage: uni [options] [targets...]

Options:
  -j N, -jN            Run up to N jobs in parallel
  -k N, -kN            Keep going until N jobs fail; -k means no limit
  -l N, -lN            Start jobs only while load average is below N
  --batch-size=N       Set targets per segment (350-4096; default 4096)
  --static             Use a fixed schedule (compatibility option)
  --plan               Print the schedule without running Ninja
  --trust-output       Skip recovered output validation
  --assume-existing    Accept outputs missing from the Ninja log
  --force-reuse        Reuse the graph without source freshness checks
  --debug              Write a detailed report to OUT_DIR (default)
  --no-debug           Disable the detailed report for this run
  --clean-logs         Remove build logs without touching build outputs
  --tui                Use the compact terminal UI (default in a TTY)
  --no-tui             Use normal line output instead of the compact TUI
  --dev                Rebuild the R8 index for this run
  --dev-auto           Enable automatic R8 index refresh
  --no-dev-auto        Disable automatic R8 index refresh
  showcommands         Print executed build commands
  -h, --help           Show this help

Examples:
  uni
  uni -j8 otapackage
  uni SystemUI
  uni --debug -j8 otapackage
`

const usageChinese = `用法: uni [选项] [目标...]

选项:
  -j N, -jN            最多并行执行 N 个任务
  -k N, -kN            失败 N 个任务后停止；-k 表示不限制
  -l N, -lN            系统负载低于 N 时才启动新任务
  --batch-size=N       设置每段目标数（350-4096，默认 4096）
  --static             使用固定调度（兼容选项）
  --plan               输出调度计划，不执行 Ninja
  --trust-output       跳过恢复产物校验
  --assume-existing    接受 Ninja 日志中缺失的已有产物
  --force-reuse        跳过源码新鲜度检查并复用构建图
  --debug              在 OUT_DIR 写入详细调试报告（默认开启）
  --no-debug           本次关闭详细调试报告
  --clean-logs         清理构建日志，不删除编译产物
  --tui                使用 compact TUI（TTY 中默认开启）
  --no-tui             禁用 compact TUI，使用普通输出
  --dev                本次重新生成 R8 索引
  --dev-auto           开启 R8 索引自动刷新
  --no-dev-auto        关闭 R8 索引自动刷新
  showcommands         输出实际执行的构建命令
  -h, --help           显示帮助

示例:
  uni
  uni -j8 otapackage
  uni SystemUI
  uni --debug -j8 otapackage
`

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

func normalizeSingleDashOptions(args []string) []string {
	aliases := map[string]string{
		"-batch-size":      "--batch-size",
		"-static":          "--static",
		"-plan":            "--plan",
		"-trust-output":    "--trust-output",
		"-assume-existing": "--assume-existing",
		"-force-reuse":     "--force-reuse",
		"-debug":           "--debug",
		"-no-debug":        "--no-debug",
		"-dev":             "--dev",
		"-dev-auto":        "--dev-auto",
		"-no-dev-auto":     "--no-dev-auto",
		"-load-average":    "--load-average",
		"-clean-logs":      "--clean-logs",
		"-tui":             "--tui",
		"-no-tui":          "--no-tui",
	}
	normalized := append([]string(nil), args...)
	for index, arg := range normalized {
		name := arg
		suffix := ""
		if separator := strings.IndexByte(name, '='); separator >= 0 {
			suffix = name[separator:]
			name = name[:separator]
		}
		if replacement, ok := aliases[name]; ok {
			normalized[index] = replacement + suffix
		}
	}
	return normalized
}

func ParseOptions(args []string) (Options, error) {
	options := Options{
		BatchSize: defaultBatchSize,
		KeepGoing: 1,
		RawArgs:   append([]string(nil), args...),
		Debug:     true,
	}
	args = normalizeSingleDashOptions(args)
	legacyOptions := map[string]string{
		"--uni-static":          "--static",
		"--uni-plan":            "--plan",
		"--uni-trust-output":    "--trust-output",
		"--uni-assume-existing": "--assume-existing",
		"--uni-force-reuse":     "--force-reuse",
		"--uni-batch-size":      "--batch-size",
		"-dev_1":                "--dev-auto",
		"-dev_0":                "--no-dev-auto",
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		legacyName := arg
		if index := strings.IndexByte(legacyName, '='); index >= 0 {
			legacyName = legacyName[:index]
		}
		if replacement, legacy := legacyOptions[legacyName]; legacy {
			return Options{}, fmt.Errorf("%s was renamed to %s", legacyName, replacement)
		}
		switch arg {
		case "-h", "-help", "--help":
			options.Help = true
			continue
		case "--static":
			options.Static = true
			continue
		case "--plan":
			options.Plan = true
			continue
		case "--trust-output":
			options.TrustOutput = true
			continue
		case "--assume-existing":
			options.AssumeExisting = true
			options.TrustOutput = true
			continue
		case "--force-reuse":
			options.ForceReuse = true
			continue
		case "--debug":
			options.Debug = true
			continue
		case "--no-debug":
			options.Debug = false
			continue
		case "--clean-logs":
			options.CleanLogs = true
			continue
		case "--tui":
			options.NoTUI = false
			continue
		case "--no-tui":
			options.NoTUI = true
			continue
		case "--dev":
			options.Dev = true
			continue
		case "--dev-auto":
			options.DevAuto = true
			options.DevAutoSet = true
			continue
		case "--no-dev-auto":
			options.DevAuto = false
			options.DevAutoSet = true
			continue
		case "showcommands":
			options.ShowCommands = true
			continue
		}
		if value, matched, err := customValue(args, &i, "--batch-size"); matched {
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

	if options.CleanLogs && len(options.BuildArgs) > 0 {
		return Options{}, fmt.Errorf("--clean-logs cannot be combined with build arguments")
	}
	if options.CleanLogs {
		options.FullBuild = false
	} else if len(options.Targets) == 0 {
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
		return Options{}, fmt.Errorf("--dev and --dev-auto cannot be used together")
	}
	return options, nil
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

func addNinjaLoadLimit(options Options, keyValues []string) []string {
	if !options.LoadSet {
		return keyValues
	}
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
	argument := "-l " + strconv.FormatFloat(options.LoadAverage, 'f', -1, 64)
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
	keyValues = addNinjaLoadLimit(options, keyValues)
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
