/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package uni

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const sourceGraphCacheVersion = 1

type sourceGraphCacheEntry struct {
	Size        int64  `json:"size"`
	ModTimeNano int64  `json:"mod_time_nano"`
	Mode        uint32 `json:"mode"`
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
}

type sourceGraphCache struct {
	Version    int                              `json:"version"`
	SourceRoot string                           `json:"source_root"`
	OutDir     string                           `json:"out_dir"`
	Dirs       map[string]sourceGraphCacheEntry `json:"directories"`
	Files      map[string]sourceGraphCacheEntry `json:"files"`
}

type sourceGraphScanStats struct {
	Entries         int64
	GraphInputs     int64
	CheckedDirs     int64
	CheckedFiles    int64
	DirectoriesRead int64
	ChangedDirs     int64
	UsedCache       bool
}

type sourceGraphChild struct {
	name      string
	relative  string
	directory bool
}

func sourceGraphFile(relative, name string) bool {
	if name == "Android.bp" || name == "Blueprints" || strings.HasSuffix(name, ".mk") {
		return true
	}
	return strings.HasPrefix(relative, "build/soong/") ||
		strings.HasPrefix(relative, "vendor/uwu/build/soong/") ||
		strings.HasPrefix(relative, "build/blueprint/")
}

func sourceGraphDirectorySkipped(path, name, outDir string) bool {
	return filepath.Clean(path) == outDir || name == ".repo" || name == ".git" || name == ".codegraph"
}

func sourceGraphFingerprint(sourceRoot, outDir string) (string, int64, error) {
	return sourceGraphFingerprintWithProgress(sourceRoot, outDir, false)
}

func sourceGraphFingerprintWithProgress(sourceRoot, outDir string, progress bool) (string, int64, error) {
	started := time.Now()
	root := filepath.Clean(sourceRoot)
	outDir = filepath.Clean(outDir)
	if root == outDir {
		hash := sha256.New()
		fmt.Fprintf(hash, "version\x00%d\n", sourceFingerprintVersion)
		return hex.EncodeToString(hash.Sum(nil)), 0, nil
	}
	cachePath := filepath.Join(outDir, "uni", ".source-graph-cache.json")
	cache, cacheErr := loadSourceGraphCache(cachePath, root, outDir)
	var next sourceGraphCache
	var stats sourceGraphScanStats
	var err error
	if cacheErr == nil {
		next, stats, err = refreshSourceGraphCache(root, outDir, cache, started, progress)
	} else {
		next, stats, err = scanSourceGraphFromScratch(root, outDir, started, progress)
	}
	if err != nil {
		if progress {
			fmt.Printf("uni: source input scan failed after %s: %v\n", time.Since(started).Round(time.Millisecond), err)
		}
		return "", 0, err
	}
	fingerprint, newest, err := fingerprintSourceGraphCache(next)
	if err != nil {
		return "", 0, err
	}
	if cacheErr != nil || !sourceGraphCachesEqual(cache, next) {
		if saveErr := saveSourceGraphCache(cachePath, next); saveErr != nil && progress {
			fmt.Fprintf(os.Stderr, "uni: source input cache could not be saved: %v\n", saveErr)
		}
	}
	if progress {
		elapsed := time.Since(started).Round(time.Millisecond)
		if stats.UsedCache {
			fmt.Printf("uni: source input cache checked %d directories and %d graph inputs, reread %d directories, %d changed (%s)\n",
				stats.CheckedDirs, stats.CheckedFiles, stats.DirectoriesRead, stats.ChangedDirs, elapsed)
		} else {
			fmt.Printf("uni: source input cache initialized: %d entries, %d graph inputs (%s)\n",
				stats.Entries, stats.GraphInputs, elapsed)
		}
	}
	return fingerprint, newest, nil
}

func loadSourceGraphCache(path, sourceRoot, outDir string) (sourceGraphCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sourceGraphCache{}, err
	}
	var cache sourceGraphCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return sourceGraphCache{}, err
	}
	if cache.Version != sourceGraphCacheVersion || cache.SourceRoot != sourceRoot || cache.OutDir != outDir ||
		cache.Dirs == nil || cache.Files == nil {
		return sourceGraphCache{}, errors.New("source input cache identity or version changed")
	}
	if _, exists := cache.Dirs["."]; !exists {
		return sourceGraphCache{}, errors.New("source input cache has no source root")
	}
	outRelative, outErr := filepath.Rel(sourceRoot, outDir)
	if outErr != nil || filepath.IsAbs(outRelative) || outRelative == ".." ||
		strings.HasPrefix(outRelative, ".."+string(filepath.Separator)) {
		outRelative = ""
	} else {
		outRelative = filepath.ToSlash(outRelative)
	}
	for relative := range cache.Dirs {
		if relative != "." && !validSourceGraphCachePath(relative, outRelative) {
			return sourceGraphCache{}, fmt.Errorf("source input cache has invalid directory %q", relative)
		}
		if relative != "." {
			parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
			if parent == "" {
				parent = "."
			}
			if _, exists := cache.Dirs[parent]; !exists {
				return sourceGraphCache{}, fmt.Errorf("source input cache has no parent for directory %q", relative)
			}
		}
	}
	for relative := range cache.Files {
		if !validSourceGraphCachePath(relative, outRelative) || !sourceGraphFile(relative, filepath.Base(filepath.FromSlash(relative))) {
			return sourceGraphCache{}, fmt.Errorf("source input cache has invalid file %q", relative)
		}
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
		if parent == "" {
			parent = "."
		}
		if _, exists := cache.Dirs[parent]; !exists {
			return sourceGraphCache{}, fmt.Errorf("source input cache has no parent for file %q", relative)
		}
	}
	return cache, nil
}

func validSourceGraphCachePath(relative, outRelative string) bool {
	if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if clean != relative || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	if outRelative != "" && (clean == outRelative || strings.HasPrefix(clean, outRelative+"/")) {
		return false
	}
	for _, component := range strings.Split(clean, "/") {
		if component == ".repo" || component == ".git" || component == ".codegraph" {
			return false
		}
	}
	return true
}

func sourceGraphMetadata(info os.FileInfo) sourceGraphCacheEntry {
	entry := sourceGraphCacheEntry{
		Size:        info.Size(),
		ModTimeNano: info.ModTime().UnixNano(),
		Mode:        uint32(info.Mode()),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		entry.Device = uint64(stat.Dev)
		entry.Inode = stat.Ino
	}
	return entry
}

func saveSourceGraphCache(path string, cache sourceGraphCache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".source-graph-cache-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0666); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func sourceGraphCachesEqual(left, right sourceGraphCache) bool {
	if left.Version != right.Version || left.SourceRoot != right.SourceRoot || left.OutDir != right.OutDir ||
		len(left.Dirs) != len(right.Dirs) || len(left.Files) != len(right.Files) {
		return false
	}
	for path, entry := range left.Dirs {
		if right.Dirs[path] != entry {
			return false
		}
	}
	for path, entry := range left.Files {
		if right.Files[path] != entry {
			return false
		}
	}
	return true
}

func sourceGraphChildren(cache sourceGraphCache) map[string][]sourceGraphChild {
	children := make(map[string][]sourceGraphChild)
	appendChild := func(relative string, directory bool) {
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
		if parent == "" {
			parent = "."
		}
		children[parent] = append(children[parent], sourceGraphChild{
			name: filepath.Base(filepath.FromSlash(relative)), relative: relative, directory: directory,
		})
	}
	for relative := range cache.Dirs {
		if relative != "." {
			appendChild(relative, true)
		}
	}
	for relative := range cache.Files {
		appendChild(relative, false)
	}
	return children
}

func sourceGraphRelative(parent, name string) string {
	if parent == "." {
		return filepath.ToSlash(name)
	}
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(parent), name))
}

func refreshSourceGraphCache(sourceRoot, outDir string, old sourceGraphCache, started time.Time, progress bool) (sourceGraphCache, sourceGraphScanStats, error) {
	next := sourceGraphCache{
		Version: sourceGraphCacheVersion, SourceRoot: sourceRoot, OutDir: outDir,
		Dirs:  make(map[string]sourceGraphCacheEntry, len(old.Dirs)),
		Files: make(map[string]sourceGraphCacheEntry, len(old.Files)),
	}
	children := sourceGraphChildren(old)
	stats := sourceGraphScanStats{UsedCache: true}
	lastProgress := started
	var scanDirectory func(string) error
	scanDirectory = func(relative string) error {
		path := sourceRoot
		if relative != "." {
			path = filepath.Join(sourceRoot, filepath.FromSlash(relative))
		}
		info, err := os.Lstat(path)
		stats.CheckedDirs++
		if progress && time.Since(lastProgress) >= 10*time.Second {
			fmt.Printf("uni: checking cached source inputs: %d directories, %d files checked, %d directories reread (%s)\n",
				stats.CheckedDirs, stats.CheckedFiles, stats.DirectoriesRead, time.Since(started).Round(time.Second))
			lastProgress = time.Now()
		}
		if errors.Is(err, os.ErrNotExist) {
			if relative == "." {
				return err
			}
			stats.ChangedDirs++
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			stats.ChangedDirs++
			return nil
		}
		metadata := sourceGraphMetadata(info)
		previous, cached := old.Dirs[relative]
		unchanged := cached && previous == metadata
		next.Dirs[relative] = metadata
		if unchanged {
			for _, child := range children[relative] {
				if child.directory {
					if err := scanDirectory(child.relative); err != nil {
						return err
					}
					continue
				}
				filePath := filepath.Join(sourceRoot, filepath.FromSlash(child.relative))
				fileInfo, err := os.Lstat(filePath)
				stats.CheckedFiles++
				if errors.Is(err, os.ErrNotExist) {
					stats.ChangedDirs++
					continue
				}
				if err != nil {
					return err
				}
				if fileInfo.IsDir() {
					stats.ChangedDirs++
					continue
				}
				next.Files[child.relative] = sourceGraphMetadata(fileInfo)
			}
			return nil
		}

		stats.ChangedDirs++
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		stats.DirectoriesRead++
		for _, entry := range entries {
			childPath := filepath.Join(path, entry.Name())
			childRelative := sourceGraphRelative(relative, entry.Name())
			if entry.IsDir() {
				if sourceGraphDirectorySkipped(childPath, entry.Name(), outDir) {
					continue
				}
				if err := scanDirectory(childRelative); err != nil {
					return err
				}
				continue
			}
			if !sourceGraphFile(childRelative, entry.Name()) {
				continue
			}
			fileInfo, err := entry.Info()
			stats.CheckedFiles++
			if err != nil {
				return err
			}
			next.Files[childRelative] = sourceGraphMetadata(fileInfo)
		}
		return nil
	}
	if err := scanDirectory("."); err != nil {
		return sourceGraphCache{}, stats, err
	}
	return next, stats, nil
}

func scanSourceGraphFromScratch(sourceRoot, outDir string, started time.Time, progress bool) (sourceGraphCache, sourceGraphScanStats, error) {
	cache := sourceGraphCache{
		Version: sourceGraphCacheVersion, SourceRoot: sourceRoot, OutDir: outDir,
		Dirs: make(map[string]sourceGraphCacheEntry), Files: make(map[string]sourceGraphCacheEntry),
	}
	stats := sourceGraphScanStats{}
	lastProgress := started
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		stats.Entries++
		if progress && time.Since(lastProgress) >= 10*time.Second {
			fmt.Printf("uni: checking source inputs: scanned %d entries, matched %d graph inputs (%s)\n",
				stats.Entries, stats.GraphInputs, time.Since(started).Round(time.Second))
			lastProgress = time.Now()
		}
		clean := filepath.Clean(path)
		if entry.IsDir() {
			if sourceGraphDirectorySkipped(clean, entry.Name(), outDir) {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(sourceRoot, clean)
			if err != nil {
				return err
			}
			cache.Dirs[filepath.ToSlash(relative)] = sourceGraphMetadata(info)
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
		cache.Files[filepath.ToSlash(relative)] = sourceGraphMetadata(info)
		stats.GraphInputs++
		return nil
	})
	if err != nil {
		return sourceGraphCache{}, stats, err
	}
	return cache, stats, nil
}

func fingerprintSourceGraphCache(cache sourceGraphCache) (string, int64, error) {
	hash := sha256.New()
	fmt.Fprintf(hash, "version\x00%d\n", sourceFingerprintVersion)
	children := sourceGraphChildren(cache)
	var newest int64
	var writeDirectory func(string) error
	writeDirectory = func(relative string) error {
		metadata, exists := cache.Dirs[relative]
		if !exists {
			return fmt.Errorf("source input cache is missing directory %q", relative)
		}
		fmt.Fprintf(hash, "dir\x00%s\x00%d\n", relative, metadata.ModTimeNano)
		entries := children[relative]
		sort.Slice(entries, func(left, right int) bool { return entries[left].name < entries[right].name })
		for _, child := range entries {
			if child.directory {
				if err := writeDirectory(child.relative); err != nil {
					return err
				}
				continue
			}
			fileMetadata, exists := cache.Files[child.relative]
			if !exists {
				return fmt.Errorf("source input cache is missing file %q", child.relative)
			}
			fmt.Fprintf(hash, "%s\x00%d\x00%d\n", child.relative, fileMetadata.Size, fileMetadata.ModTimeNano)
			if fileMetadata.ModTimeNano > newest {
				newest = fileMetadata.ModTimeNano
			}
		}
		return nil
	}
	if err := writeDirectory("."); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), newest, nil
}
