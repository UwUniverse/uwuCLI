/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package uni

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func legacySourceGraphFingerprint(sourceRoot, outDir string) (string, int64, error) {
	hash := sha256.New()
	fmt.Fprintf(hash, "version\x00%d\n", sourceFingerprintVersion)
	var newest int64
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		clean := filepath.Clean(path)
		if entry.IsDir() {
			if sourceGraphDirectorySkipped(clean, entry.Name(), filepath.Clean(outDir)) {
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
			fmt.Fprintf(hash, "dir\x00%s\x00%d\n", filepath.ToSlash(relative), info.ModTime().UnixNano())
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

func TestSourceGraphFingerprintCacheMatchesLegacyWalkAndRefreshesChanges(t *testing.T) {
	root := t.TempDir()
	outDir := filepath.Join(root, "out")
	for _, directory := range []string{
		filepath.Join(root, "device", "sample"),
		filepath.Join(root, "build", "soong"),
		filepath.Join(root, ".git"),
		filepath.Join(outDir, "uni"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(relative, contents string, timestamp time.Time) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
	baseTime := time.Unix(1_800_000_000, 0)
	write("device/sample/Android.bp", "filegroup { name: \"base\" }\n", baseTime)
	write("build/soong/graph_input.go", "package soong\n", baseTime)
	write("device/sample/ignored.txt", "ignored\n", baseTime)
	write(".git/Android.bp", "ignored\n", baseTime)

	fingerprint, newest, err := sourceGraphFingerprint(root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyNewest, err := legacySourceGraphFingerprint(root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != legacy || newest != legacyNewest {
		t.Fatalf("initial cached fingerprint differs from legacy walk: %s/%d != %s/%d", fingerprint, newest, legacy, legacyNewest)
	}

	cache, err := loadSourceGraphCache(filepath.Join(outDir, "uni", ".source-graph-cache.json"), root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, stats, err := refreshSourceGraphCache(root, outDir, cache, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DirectoriesRead != 0 || !stats.UsedCache {
		t.Fatalf("unchanged tree reread directories: stats=%+v", stats)
	}
	second, secondNewest, err := fingerprintSourceGraphCache(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if second != legacy || secondNewest != legacyNewest {
		t.Fatalf("unchanged incremental fingerprint differs: %s/%d != %s/%d", second, secondNewest, legacy, legacyNewest)
	}

	write("device/sample/Android.bp", "filegroup { name: \"updated\" }\n", baseTime.Add(time.Minute))
	write("device/sample/New.mk", "PRODUCT_PACKAGES += new\n", baseTime.Add(2*time.Minute))
	updated, stats, err := refreshSourceGraphCache(root, outDir, refreshed, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DirectoriesRead == 0 {
		t.Fatal("changed source directory was not reread")
	}
	updatedFingerprint, updatedNewest, err := fingerprintSourceGraphCache(updated)
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyNewest, err = legacySourceGraphFingerprint(root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if updatedFingerprint != legacy || updatedNewest != legacyNewest {
		t.Fatalf("updated incremental fingerprint differs: %s/%d != %s/%d", updatedFingerprint, updatedNewest, legacy, legacyNewest)
	}

	write("device/sample/nested/Android.bp", "filegroup { name: \"nested\" }\n", baseTime.Add(3*time.Minute))
	nested, stats, err := refreshSourceGraphCache(root, outDir, updated, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DirectoriesRead == 0 {
		t.Fatal("new nested source directory was not scanned")
	}
	nestedFingerprint, nestedNewest, err := fingerprintSourceGraphCache(nested)
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyNewest, err = legacySourceGraphFingerprint(root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if nestedFingerprint != legacy || nestedNewest != legacyNewest {
		t.Fatalf("new-directory incremental fingerprint differs: %s/%d != %s/%d", nestedFingerprint, nestedNewest, legacy, legacyNewest)
	}

	if err := os.Remove(filepath.Join(root, "device/sample/New.mk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "device/sample/nested/Android.bp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "device/sample/nested")); err != nil {
		t.Fatal(err)
	}
	deleted, _, err := refreshSourceGraphCache(root, outDir, nested, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	deletedFingerprint, deletedNewest, err := fingerprintSourceGraphCache(deleted)
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyNewest, err = legacySourceGraphFingerprint(root, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if deletedFingerprint != legacy || deletedNewest != legacyNewest {
		t.Fatalf("deleted incremental fingerprint differs: %s/%d != %s/%d", deletedFingerprint, deletedNewest, legacy, legacyNewest)
	}
}
