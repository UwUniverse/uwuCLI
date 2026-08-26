// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const stateVersion = 7

const r8CacheVersion = 1

type R8IndexMode int

const (
	R8IndexFast R8IndexMode = iota
	R8IndexFull
	R8IndexAuto
)

type GraphFile struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ModTimeNano int64  `json:"mod_time_nano"`
}

type State struct {
	Version           int         `json:"version"`
	SourceRoot        string      `json:"source_root"`
	OutDir            string      `json:"out_dir"`
	ProductOut        string      `json:"product_out"`
	TargetProduct     string      `json:"target_product"`
	TargetDevice      string      `json:"target_device"`
	TargetRelease     string      `json:"target_release"`
	BuildVariant      string      `json:"build_variant"`
	BuildDateTime     string      `json:"build_date_time"`
	BuildDateTimeFile string      `json:"build_date_time_file"`
	KatiSuffix        string      `json:"kati_suffix"`
	CombinedNinja     string      `json:"combined_ninja"`
	SoongNinja        string      `json:"soong_ninja"`
	SoongVariables    string      `json:"soong_variables"`
	KatiEnvironment   string      `json:"kati_environment"`
	KatiBuildNinja    string      `json:"kati_build_ninja"`
	KatiPackageNinja  string      `json:"kati_package_ninja"`
	OriginalArgs      []string    `json:"original_args"`
	NinjaArgs         []string    `json:"ninja_args"`
	ProductPackages   []string    `json:"product_packages"`
	AllModules        []string    `json:"all_modules"`
	R8Modules         []string    `json:"r8_modules"`
	R8ModulesReady    bool        `json:"r8_modules_ready"`
	Dist              bool        `json:"dist"`
	GraphFingerprint  string      `json:"graph_fingerprint"`
	GraphFiles        []GraphFile `json:"graph_files"`
	SourceFingerprint string      `json:"source_fingerprint,omitempty"`
}

func SaveState(path string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0666); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

func graphFingerprint(files []GraphFile) (string, error) {
	hash := sha256.New()
	for _, expected := range files {
		info, err := os.Stat(expected.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", expected.Path, info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (state State) Validate(sourceRoot, outDir, product string) error {
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.BuildDateTime == "" || state.BuildDateTimeFile == "" {
		return fmt.Errorf("state is missing build date information")
	}
	if filepath.Clean(state.SourceRoot) != filepath.Clean(sourceRoot) {
		return fmt.Errorf("source root changed")
	}
	if filepath.Clean(state.OutDir) != filepath.Clean(outDir) {
		return fmt.Errorf("output directory changed")
	}
	if state.TargetProduct != product {
		return fmt.Errorf("target product changed")
	}
	fingerprint, err := graphFingerprint(state.GraphFiles)
	if err != nil {
		return fmt.Errorf("build graph is incomplete: %w", err)
	}
	if fingerprint != state.GraphFingerprint {
		return fmt.Errorf("build graph changed")
	}
	return nil
}

func sourceGraphFile(relative, name string) bool {
	if name == "Android.bp" || name == "Blueprints" || strings.HasSuffix(name, ".mk") {
		return true
	}
	return strings.HasPrefix(relative, "build/soong/") ||
		strings.HasPrefix(relative, "build/blueprint/")
}

func sourceGraphFingerprint(sourceRoot, outDir string) (string, int64, error) {
	hash := sha256.New()
	var newest int64
	outDir = filepath.Clean(outDir)
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		clean := filepath.Clean(path)
		if entry.IsDir() {
			if clean == outDir || entry.Name() == ".repo" || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, clean)
		if err != nil || !sourceGraphFile(filepath.ToSlash(relative), entry.Name()) {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", filepath.ToSlash(relative), info.Size(), info.ModTime().UnixNano())
		if info.ModTime().UnixNano() > newest {
			newest = info.ModTime().UnixNano()
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), newest, nil
}

func (state State) validateReusableGraph(sourceRoot, outDir, product string) (int64, error) {
	if state.Version != stateVersion {
		return 0, fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.BuildDateTime == "" || state.BuildDateTimeFile == "" {
		return 0, fmt.Errorf("state is missing build date information")
	}
	if filepath.Clean(state.SourceRoot) != filepath.Clean(sourceRoot) ||
		filepath.Clean(state.OutDir) != filepath.Clean(outDir) || state.TargetProduct != product {
		return 0, fmt.Errorf("build identity changed")
	}
	var oldest int64
	for _, expected := range state.GraphFiles {
		if filepath.Clean(expected.Path) == filepath.Clean(state.BuildDateTimeFile) {
			continue
		}
		info, err := os.Stat(expected.Path)
		if err != nil {
			return 0, err
		}
		if info.Size() != expected.Size || info.ModTime().UnixNano() != expected.ModTimeNano {
			return 0, fmt.Errorf("build graph changed")
		}
		if oldest == 0 || info.ModTime().UnixNano() < oldest {
			oldest = info.ModTime().UnixNano()
		}
	}
	if oldest == 0 {
		return 0, fmt.Errorf("state has no reusable graph")
	}
	return oldest, nil
}

func currentNinjaTargets(options Options) []string {
	if len(options.Targets) == 0 {
		return []string{"droid"}
	}
	return append([]string(nil), options.Targets...)
}

func ReuseState(path, sourceRoot, outDir, product, release, variant string, options Options) (State, bool, error) {
	if len(options.KeyValues) != 0 {
		return State{}, false, nil
	}
	state, err := LoadState(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	oldest, err := state.validateReusableGraph(sourceRoot, outDir, product)
	if err != nil || state.TargetRelease != release || state.BuildVariant != variant {
		return State{}, false, nil
	}
	fingerprint, newest, err := sourceGraphFingerprint(sourceRoot, outDir)
	if err != nil {
		return State{}, false, err
	}
	if state.SourceFingerprint != "" {
		if fingerprint != state.SourceFingerprint {
			return State{}, false, nil
		}
	} else if newest > oldest {
		return State{}, false, nil
	}
	buildDate := []byte(state.BuildDateTime + "\n")
	current, readErr := os.ReadFile(state.BuildDateTimeFile)
	if readErr != nil || !bytes.Equal(current, buildDate) {
		if err := os.WriteFile(state.BuildDateTimeFile, buildDate, 0666); err != nil {
			return State{}, false, err
		}
	}
	state.NinjaArgs = currentNinjaTargets(options)
	state.OriginalArgs = append([]string(nil), options.BuildArgs...)
	state.Dist = options.Dist
	state.SourceFingerprint = fingerprint
	for index := range state.GraphFiles {
		info, statErr := os.Stat(state.GraphFiles[index].Path)
		if statErr != nil {
			return State{}, false, statErr
		}
		state.GraphFiles[index].Size = info.Size()
		state.GraphFiles[index].ModTimeNano = info.ModTime().UnixNano()
	}
	state.GraphFingerprint, err = graphFingerprint(state.GraphFiles)
	if err != nil {
		return State{}, false, err
	}
	if err := SaveState(path, state); err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// ForceReuseState skips freshness checks and refreshes graph metadata so a
// prepared graph can be recovered after lunch or an interrupted analysis.
func ForceReuseState(path, sourceRoot, outDir, product, release, variant string, options Options) (State, bool, error) {
	if len(options.KeyValues) != 0 {
		return State{}, false, fmt.Errorf("--uni-force-reuse cannot be combined with product variable overrides")
	}
	state, err := LoadState(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	if state.Version != stateVersion || state.BuildDateTime == "" || state.BuildDateTimeFile == "" ||
		filepath.Clean(state.SourceRoot) != filepath.Clean(sourceRoot) ||
		filepath.Clean(state.OutDir) != filepath.Clean(outDir) || state.TargetProduct != product {
		return State{}, false, nil
	}
	if state.TargetRelease != release || state.BuildVariant != variant {
		return State{}, false, nil
	}
	state.NinjaArgs = currentNinjaTargets(options)
	state.OriginalArgs = append([]string(nil), options.BuildArgs...)
	state.Dist = options.Dist
	for index := range state.GraphFiles {
		info, statErr := os.Stat(state.GraphFiles[index].Path)
		if statErr != nil {
			return State{}, false, fmt.Errorf("prepared graph file missing: %w", statErr)
		}
		state.GraphFiles[index].Size = info.Size()
		state.GraphFiles[index].ModTimeNano = info.ModTime().UnixNano()
	}
	state.GraphFingerprint, err = graphFingerprint(state.GraphFiles)
	if err != nil {
		return State{}, false, err
	}
	if err := SaveState(path, state); err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

func RecordSourceFingerprint(path, sourceRoot, outDir string, state State) (State, error) {
	fingerprint, _, err := sourceGraphFingerprint(sourceRoot, outDir)
	if err != nil {
		return State{}, err
	}
	state.SourceFingerprint = fingerprint
	if err := SaveState(path, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func ProductTargets(state State) []string {
	modules := make(map[string]struct{}, len(state.AllModules))
	for _, module := range state.AllModules {
		modules[module] = struct{}{}
	}
	seen := make(map[string]struct{}, len(state.ProductPackages))
	targets := make([]string, 0, len(state.ProductPackages))
	for _, target := range state.ProductPackages {
		if _, ok := modules[target]; !ok {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
}

func StableShuffle(targets []string, seed string) []string {
	type rankedTarget struct {
		name string
		rank [32]byte
	}
	ranked := make([]rankedTarget, 0, len(targets))
	for _, target := range targets {
		ranked = append(ranked, rankedTarget{
			name: target,
			rank: sha256.Sum256([]byte(seed + "\x00" + target)),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return strings.Compare(string(ranked[i].rank[:]), string(ranked[j].rank[:])) < 0
	})
	result := make([]string, len(ranked))
	for i := range ranked {
		result[i] = ranked[i].name
	}
	return result
}

type ninjaMetadata struct {
	modules  []string
	children []string
	file     GraphFile
	err      error
}

type r8Cache struct {
	Version int         `json:"version"`
	Root    string      `json:"root"`
	Files   []GraphFile `json:"files"`
	Modules []string    `json:"modules"`
}

func resolveNinjaPath(sourceRoot, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(sourceRoot, path)
	}
	return filepath.Clean(path)
}

func scanNinjaMetadata(ctx context.Context, sourceRoot, path string) ninjaMetadata {
	if err := ctx.Err(); err != nil {
		return ninjaMetadata{err: err}
	}
	path = resolveNinjaPath(sourceRoot, path)
	file, err := os.Open(path)
	if err != nil {
		return ninjaMetadata{err: err}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ninjaMetadata{err: err}
	}
	result := ninjaMetadata{file: GraphFile{Path: path, Size: info.Size(), ModTimeNano: info.ModTime().UnixNano()}}
	reader := bufio.NewReaderSize(file, 1024*1024)
	atLineStart := true
	for {
		if err := ctx.Err(); err != nil {
			result.err = err
			break
		}
		line, readErr := reader.ReadSlice('\n')
		if atLineStart {
			parseNinjaMetadataLine(bytes.TrimSpace(line), &result)
		}
		if readErr == bufio.ErrBufferFull {
			atLineStart = false
			continue
		}
		atLineStart = true
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.err = readErr
			break
		}
	}
	return result
}

func parseNinjaMetadataLine(line []byte, result *ninjaMetadata) {
	if child, found := bytes.CutPrefix(line, []byte("subninja ")); found {
		child = bytes.TrimSpace(child)
		if len(child) > 0 && !bytes.ContainsRune(child, '$') {
			result.children = append(result.children, string(child))
		}
		return
	}
	fields, found := bytes.CutPrefix(line, []byte("tags = "))
	if !found {
		return
	}
	var module []byte
	r8 := false
	for _, field := range bytes.Split(fields, []byte(";")) {
		key, value, found := bytes.Cut(field, []byte("="))
		if !found {
			continue
		}
		switch string(key) {
		case "module_name":
			module = value
		case "rule_name":
			r8 = bytes.Equal(value, []byte("r8")) ||
				bytes.Equal(value, []byte("r8RE")) ||
				bytes.Equal(value, []byte("d8r8")) ||
				bytes.Equal(value, []byte("d8r8RE")) ||
				bytes.Equal(value, []byte("d8Incr8")) ||
				bytes.Equal(value, []byte("d8Incr8RE"))
		}
	}
	if r8 && len(module) > 0 {
		result.modules = append(result.modules, string(module))
	}
}

func findSoongNinja(ctx context.Context, state State) (string, error) {
	metadata := scanNinjaMetadata(ctx, state.SourceRoot, state.CombinedNinja)
	if metadata.err != nil {
		return "", metadata.err
	}
	for _, child := range metadata.children {
		path := filepath.ToSlash(child)
		if strings.Contains(path, "/soong/") || strings.HasPrefix(path, "out/soong/") {
			return child, nil
		}
	}
	return "", fmt.Errorf("Soong Ninja file is not included by %s", state.CombinedNinja)
}

func scanR8Modules(ctx context.Context, state State) (map[string]struct{}, []GraphFile, error) {
	root := state.SoongNinja
	if root == "" {
		var err error
		root, err = findSoongNinja(ctx, state)
		if err != nil {
			return nil, nil, err
		}
	}
	modules := make(map[string]struct{})
	var files []GraphFile
	visited := make(map[string]struct{})
	current := []string{root}
	for len(current) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		paths := make([]string, 0, len(current))
		for _, path := range current {
			path = resolveNinjaPath(state.SourceRoot, path)
			if _, ok := visited[path]; ok {
				continue
			}
			visited[path] = struct{}{}
			paths = append(paths, path)
		}
		if len(paths) == 0 {
			break
		}
		workers := min(4, len(paths))
		jobs := make(chan string)
		results := make(chan ninjaMetadata, len(paths))
		var group sync.WaitGroup
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				for path := range jobs {
					results <- scanNinjaMetadata(ctx, state.SourceRoot, path)
				}
			}()
		}
		go func() {
			for _, path := range paths {
				jobs <- path
			}
			close(jobs)
			group.Wait()
			close(results)
		}()
		current = nil
		for result := range results {
			if result.err != nil {
				return nil, nil, result.err
			}
			files = append(files, result.file)
			for _, module := range result.modules {
				modules[module] = struct{}{}
			}
			current = append(current, result.children...)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return modules, files, nil
}

func DetectR8Modules(state State) (map[string]struct{}, error) {
	modules, _, err := scanR8Modules(context.Background(), state)
	return modules, err
}

func cachedR8Modules(ctx context.Context, state State, path string) (map[string]struct{}, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cache r8Cache
	if json.Unmarshal(data, &cache) != nil || cache.Version != r8CacheVersion {
		return nil, false
	}
	root := resolveNinjaPath(state.SourceRoot, state.SoongNinja)
	if filepath.Clean(cache.Root) != root || len(cache.Files) == 0 {
		return nil, false
	}
	for _, file := range cache.Files {
		if ctx.Err() != nil {
			return nil, false
		}
		info, err := os.Stat(file.Path)
		if err != nil || info.Size() != file.Size || info.ModTime().UnixNano() != file.ModTimeNano {
			return nil, false
		}
	}
	modules := make(map[string]struct{}, len(cache.Modules))
	for _, module := range cache.Modules {
		modules[module] = struct{}{}
	}
	return modules, true
}

func writeR8Cache(path, root string, files []GraphFile, modules map[string]struct{}) error {
	names := make([]string, 0, len(modules))
	for module := range modules {
		names = append(names, module)
	}
	sort.Strings(names)
	data, err := json.Marshal(r8Cache{Version: r8CacheVersion, Root: root, Files: files, Modules: names})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".r8-modules-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func LoadR8ModulesForModeContext(ctx context.Context, state State, path string, mode R8IndexMode) (map[string]struct{}, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}
	if mode != R8IndexFull {
		if modules, ok := cachedR8Modules(ctx, state, path); ok {
			return modules, "full-scan-cache", nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}
	if mode == R8IndexFast {
		modules := make(map[string]struct{}, len(state.R8Modules))
		if state.R8ModulesReady {
			for _, module := range state.R8Modules {
				modules[module] = struct{}{}
			}
			return modules, "soong-fast", nil
		}
		return modules, "fast-scan-unavailable", nil
	}
	modules, files, err := scanR8Modules(ctx, state)
	if err != nil {
		return nil, "ninja-full-scan", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "ninja-full-scan", err
	}
	root := resolveNinjaPath(state.SourceRoot, state.SoongNinja)
	if err := writeR8Cache(path, root, files, modules); err != nil {
		return nil, "ninja-full-scan", err
	}
	return modules, "ninja-full-scan", nil
}

func LoadR8ModulesDetailedContext(ctx context.Context, state State, path string, fullScan bool) (map[string]struct{}, string, error) {
	mode := R8IndexFast
	if fullScan {
		mode = R8IndexAuto
	}
	return LoadR8ModulesForModeContext(ctx, state, path, mode)
}

func LoadR8ModulesDetailed(state State, path string, fullScan bool) (map[string]struct{}, string, error) {
	return LoadR8ModulesDetailedContext(context.Background(), state, path, fullScan)
}

func LoadR8Modules(state State, path string, fullScan bool) (map[string]struct{}, error) {
	modules, _, err := LoadR8ModulesDetailed(state, path, fullScan)
	return modules, err
}

func InterleaveR8Targets(targets []string, r8Modules map[string]struct{}) []string {
	r8Targets := make([]string, 0)
	otherTargets := make([]string, 0, len(targets))
	for _, target := range targets {
		if _, ok := r8Modules[target]; ok {
			r8Targets = append(r8Targets, target)
		} else {
			otherTargets = append(otherTargets, target)
		}
	}
	if len(r8Targets) == 0 || len(otherTargets) == 0 {
		return append([]string(nil), targets...)
	}
	result := make([]string, 0, len(targets))
	r8Index := 0
	otherIndex := 0
	for position := 0; position < len(targets); position++ {
		expectedR8 := (position*len(r8Targets) + len(targets) - 1) / len(targets)
		if r8Index < expectedR8 && r8Index < len(r8Targets) {
			result = append(result, r8Targets[r8Index])
			r8Index++
		} else if otherIndex < len(otherTargets) {
			result = append(result, otherTargets[otherIndex])
			otherIndex++
		} else {
			result = append(result, r8Targets[r8Index])
			r8Index++
		}
	}
	return result
}

func R8PrimeTargets(targets []string, r8Modules map[string]struct{}, jobs, segments int) []string {
	var candidates []string
	for _, target := range targets {
		if _, ok := r8Modules[target]; ok {
			candidates = append(candidates, target)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if jobs < 1 {
		jobs = 1
	}
	if segments < 1 {
		segments = 1
	}
	count := (len(candidates) + segments) / (segments + 1)
	count = min(count, jobs)
	count = max(1, count)
	prime := make([]string, 0, count)
	for index := 0; index < count; index++ {
		prime = append(prime, candidates[index*len(candidates)/count])
	}
	return prime
}

func R8TargetCount(targets []string, r8Modules map[string]struct{}) int {
	count := 0
	for _, target := range targets {
		if _, ok := r8Modules[target]; ok {
			count++
		}
	}
	return count
}

func Batches(targets []string, size int) [][]string {
	if size <= 0 {
		return nil
	}
	var batches [][]string
	for len(targets) > 0 {
		count := min(size, len(targets))
		batch := append([]string(nil), targets[:count]...)
		batches = append(batches, batch)
		targets = targets[count:]
	}
	return batches
}

func HasNinjaTarget(path, target string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	prefix := "build " + target + ":"
	reader := bufio.NewReaderSize(file, 1024*1024)
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, prefix) {
			return true, nil
		}
		if err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
	}
}

func EarlyKernelTarget(path string) (string, error) {
	for _, target := range []string{"kernel", "bootimage"} {
		found, err := HasNinjaTarget(path, target)
		if err != nil {
			return "", err
		}
		if found {
			return target, nil
		}
	}
	return "", nil
}
