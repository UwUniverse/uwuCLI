// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const historyVersion = 1

type historyStat struct {
	Samples int     `json:"samples"`
	EWMA    float64 `json:"ewma"`
	Maximum float64 `json:"maximum"`
}

func (stat *historyStat) observe(value float64) {
	stat.Samples++
	if stat.Samples == 1 {
		stat.EWMA = value
	} else {
		stat.EWMA = 0.3*value + 0.7*stat.EWMA
	}
	if value > stat.Maximum {
		stat.Maximum = value
	}
}

type moduleHistory struct {
	DurationMS historyStat `json:"duration_ms"`
}

type taskTypeHistory struct {
	DurationMS historyStat `json:"duration_ms"`
	RSSBytes   historyStat `json:"rss_bytes"`
}

type phaseHistory struct {
	DurationMS       historyStat `json:"duration_ms"`
	MinimumAvailable historyStat `json:"minimum_available_bytes"`
	PSISomeAvg10     historyStat `json:"psi_some_avg10_peak"`
	PSIFullAvg10     historyStat `json:"psi_full_avg10_peak"`
	SwapOutRate      historyStat `json:"swap_out_peak_bytes_per_second"`
}

type buildHistory struct {
	Version              int                         `json:"version"`
	Product              string                      `json:"product"`
	BuildSamples         int                         `json:"build_samples"`
	SourceFingerprint    string                      `json:"source_fingerprint"`
	ToolchainFingerprint string                      `json:"toolchain_fingerprint"`
	UpdatedAt            time.Time                   `json:"updated_at"`
	Modules              map[string]*moduleHistory   `json:"modules"`
	TaskTypes            map[string]*taskTypeHistory `json:"task_types"`
	Phases               map[string]*phaseHistory    `json:"phases"`
}

type taskMetadata struct {
	Version int          `json:"version"`
	Actions []taskAction `json:"actions"`
}

type taskAction struct {
	Module   string   `json:"module"`
	Rule     string   `json:"rule"`
	TaskType string   `json:"task_type"`
	Outputs  []string `json:"outputs"`
}

func historyPath(top, product string) string {
	return filepath.Join(top, ".repo", "uwuCLI", "history", product+".json")
}

func newBuildHistory(product, sourceFingerprint, toolchainFingerprint string) buildHistory {
	return buildHistory{
		Version: historyVersion, Product: product,
		SourceFingerprint: sourceFingerprint, ToolchainFingerprint: toolchainFingerprint,
		Modules: make(map[string]*moduleHistory), TaskTypes: make(map[string]*taskTypeHistory),
		Phases: make(map[string]*phaseHistory),
	}
}

func loadBuildHistory(path string) (buildHistory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return buildHistory{}, nil
	}
	if err != nil {
		return buildHistory{}, err
	}
	var history buildHistory
	if err := json.Unmarshal(data, &history); err != nil || history.Version != historyVersion {
		quarantine := path + ".corrupt-" + time.Now().Format("20060102-150405.000000000")
		if renameErr := renameAndSync(path, quarantine); renameErr != nil {
			return buildHistory{}, errors.Join(err, renameErr)
		}
		if err == nil {
			err = fmt.Errorf("unsupported history version %d", history.Version)
		}
		return buildHistory{}, fmt.Errorf("quarantined damaged history at %s: %w", quarantine, err)
	}
	if history.Modules == nil {
		history.Modules = make(map[string]*moduleHistory)
	}
	if history.TaskTypes == nil {
		history.TaskTypes = make(map[string]*taskTypeHistory)
	}
	if history.Phases == nil {
		history.Phases = make(map[string]*phaseHistory)
	}
	return history, nil
}

func saveBuildHistory(path string, history buildHistory) error {
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".history-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0666); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return renameAndSync(temporaryPath, path)
}

func toolchainFingerprint(runner *commandRunner) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "ninja=%s\n", runner.phasedNinja)
	paths := []string{runner.soongUIPath}
	if ninja, err := exec.LookPath(runner.phasedNinja); err == nil {
		paths = append(paths, ninja)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(hash, "%s\x00missing\n", path)
			continue
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(hash, "%s\x00%x\n", path, digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func loadTaskMetadata(path string) (taskMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return taskMetadata{}, err
	}
	var metadata taskMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return taskMetadata{}, err
	}
	if metadata.Version != 1 {
		return taskMetadata{}, fmt.Errorf("unsupported task metadata version %d", metadata.Version)
	}
	return metadata, nil
}

func outputKeys(path string) []string {
	clean := filepath.Clean(filepath.FromSlash(path))
	keys := []string{clean, filepath.ToSlash(clean)}
	if absolute, err := filepath.Abs(clean); err == nil {
		keys = append(keys, absolute, filepath.ToSlash(absolute))
	}
	return keys
}

func historyActionByOutput(metadata taskMetadata) map[string]taskAction {
	result := make(map[string]taskAction)
	for _, action := range metadata.Actions {
		for _, output := range action.Outputs {
			for _, key := range outputKeys(output) {
				result[key] = action
			}
		}
	}
	return result
}

func durationWeights(ninjaLogPath string, metadata taskMetadata) (map[string]float64, map[string]float64, error) {
	logData, err := readNinjaLog(ninjaLogPath)
	if err != nil {
		return nil, nil, err
	}
	actions := historyActionByOutput(metadata)
	moduleDurations := make(map[string]float64)
	taskDurations := make(map[string]float64)
	seen := make(map[string]struct{})
	for _, output := range logData.order {
		var action taskAction
		found := false
		for _, key := range outputKeys(output) {
			if action, found = actions[key]; found {
				break
			}
		}
		if !found {
			continue
		}
		fields := strings.SplitN(logData.lines[output], "\t", 5)
		if len(fields) != 5 {
			continue
		}
		started, startErr := strconv.ParseInt(fields[0], 10, 64)
		ended, endErr := strconv.ParseInt(fields[1], 10, 64)
		if startErr != nil || endErr != nil || ended < started {
			continue
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", action.Module, action.Rule, started, ended, fields[4])
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		duration := float64(ended - started)
		moduleDurations[action.Module] += duration
		taskDurations[action.TaskType] += duration
	}
	return moduleDurations, taskDurations, nil
}

func soongHintWeights(path string, metadata taskMetadata) (map[string]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actions := historyActionByOutput(metadata)
	weights := make(map[string]float64)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		separator := strings.LastIndexByte(line, ',')
		if separator <= 0 {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(line[separator+1:]), 64)
		if err != nil || weight <= 0 {
			continue
		}
		output := strings.TrimSpace(line[:separator])
		for _, key := range outputKeys(output) {
			action, found := actions[key]
			if !found || action.Module == "" {
				continue
			}
			weights[action.Module] = max(weights[action.Module], weight)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return weights, nil
}

func updateDurationHistory(history *buildHistory, ninjaLogPath string, metadata taskMetadata) error {
	moduleDurations, taskDurations, err := durationWeights(ninjaLogPath, metadata)
	if err != nil {
		return err
	}
	for module, duration := range moduleDurations {
		entry := history.Modules[module]
		if entry == nil {
			entry = &moduleHistory{}
			history.Modules[module] = entry
		}
		entry.DurationMS.observe(duration)
	}
	for taskType, duration := range taskDurations {
		entry := history.TaskTypes[taskType]
		if entry == nil {
			entry = &taskTypeHistory{}
			history.TaskTypes[taskType] = entry
		}
		entry.DurationMS.observe(duration)
	}
	return nil
}

func updateResourceHistory(history *buildHistory, summary buildSummary) {
	buildRSS := make(map[string]float64)
	for _, sample := range summary.samples {
		phase := history.Phases[sample.Phase]
		if phase == nil {
			phase = &phaseHistory{}
			history.Phases[sample.Phase] = phase
		}
		phase.DurationMS.observe(float64(sample.Duration / time.Millisecond))
		phase.MinimumAvailable.observe(float64(sample.MinimumAvailable))
		phase.PSISomeAvg10.observe(sample.MaxPSISomeAvg10)
		phase.PSIFullAvg10.observe(sample.MaxPSIFullAvg10)
		phase.SwapOutRate.observe(sample.MaxSwapOutRate)
		for _, process := range sample.TopRSS {
			buildRSS[process.TaskType] = max(buildRSS[process.TaskType], float64(process.RSSBytes))
		}
	}
	for taskType, rss := range buildRSS {
		entry := history.TaskTypes[taskType]
		if entry == nil {
			entry = &taskTypeHistory{}
			history.TaskTypes[taskType] = entry
		}
		entry.RSSBytes.observe(rss)
	}
}

func recordBuildHistory(top string, state State, runner *commandRunner, summary buildSummary) error {
	if state.TaskMetadata == "" {
		return fmt.Errorf("state has no task metadata")
	}
	metadata, err := loadTaskMetadata(state.TaskMetadata)
	if err != nil {
		return err
	}
	toolchain := toolchainFingerprint(runner)
	path := historyPath(top, state.TargetProduct)
	history, loadErr := loadBuildHistory(path)
	if loadErr != nil {
		history = buildHistory{}
	}
	if history.Version != historyVersion || history.Product != state.TargetProduct {
		history = newBuildHistory(state.TargetProduct, state.SourceFingerprint, toolchain)
	}
	if err := updateDurationHistory(&history, filepath.Join(state.OutDir, ".ninja_log"), metadata); err != nil {
		return err
	}
	updateResourceHistory(&history, summary)
	history.BuildSamples++
	history.SourceFingerprint = state.SourceFingerprint
	history.ToolchainFingerprint = toolchain
	history.UpdatedAt = time.Now()
	return saveBuildHistory(path, history)
}

func historyWeights(top string, state State, runner *commandRunner) (map[string]float64, string) {
	history, err := loadBuildHistory(historyPath(top, state.TargetProduct))
	weights := make(map[string]float64)
	if err == nil && history.Product == state.TargetProduct {
		for module, entry := range history.Modules {
			if entry.DurationMS.Samples > 0 {
				weights[module] = entry.DurationMS.EWMA
			}
		}
		if len(weights) > 0 {
			return weights, fmt.Sprintf("persistent samples=%d modules=%d", history.BuildSamples, len(weights))
		}
	}
	if state.TaskMetadata != "" {
		metadata, metadataErr := loadTaskMetadata(state.TaskMetadata)
		if metadataErr == nil {
			observed, _, logErr := durationWeights(filepath.Join(state.OutDir, ".ninja_log"), metadata)
			if logErr == nil && len(observed) > 0 {
				return observed, fmt.Sprintf("ninja-log modules=%d", len(observed))
			}
			hints, hintErr := soongHintWeights(filepath.Join(state.OutDir, ".ninja_weight_list"), metadata)
			if hintErr == nil && len(hints) > 0 {
				return hints, fmt.Sprintf("soong-hint modules=%d", len(hints))
			}
		}
	}
	if err != nil {
		return nil, err.Error()
	}
	return nil, fmt.Sprintf("observing samples=%d", history.BuildSamples)
}

func LongPrimeTargets(targets []string, weights map[string]float64, limit int) []string {
	if limit <= 0 || len(weights) == 0 {
		return nil
	}
	candidates := make([]string, 0, min(limit, len(targets)))
	for _, target := range targets {
		if weights[target] > 0 {
			candidates = append(candidates, target)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return weights[candidates[i]] > weights[candidates[j]]
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func InterleaveLongTargets(targets []string, weights map[string]float64) []string {
	if len(weights) == 0 || len(targets) < 2 {
		return append([]string(nil), targets...)
	}
	candidates := make([]string, 0)
	for _, target := range targets {
		if weights[target] > 0 {
			candidates = append(candidates, target)
		}
	}
	if len(candidates) == 0 {
		return append([]string(nil), targets...)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return weights[candidates[i]] > weights[candidates[j]] })
	longCount := min(len(candidates), max(1, (len(targets)+3)/4))
	longSet := make(map[string]struct{}, longCount)
	weighted := append([]string(nil), candidates[:longCount]...)
	for _, target := range weighted {
		longSet[target] = struct{}{}
	}
	others := make([]string, 0, len(targets)-len(weighted))
	for _, target := range targets {
		if _, isLong := longSet[target]; !isLong {
			others = append(others, target)
		}
	}
	result := make([]string, 0, len(targets))
	weightedIndex, otherIndex := 0, 0
	for position := 0; position < len(targets); position++ {
		expectedWeighted := (position*len(weighted) + len(targets) - 1) / len(targets)
		if weightedIndex < expectedWeighted && weightedIndex < len(weighted) {
			result = append(result, weighted[weightedIndex])
			weightedIndex++
		} else if otherIndex < len(others) {
			result = append(result, others[otherIndex])
			otherIndex++
		} else {
			result = append(result, weighted[weightedIndex])
			weightedIndex++
		}
	}
	return result
}
