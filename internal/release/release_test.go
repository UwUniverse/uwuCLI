// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestLoadProjects(t *testing.T) {
	top := t.TempDir()
	manifest := filepath.Join(top, "manifest.xml")
	content := `<manifest><default remote="default"/><project name="a/project" revision="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"/><project name="LineageOS/foo" revision="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" remote="lineageos"/><project name="vendor/uwu-versions" revision="cccccccccccccccccccccccccccccccccccccccc"/><project name="b" path="custom/b" revision="not-a-sha1"/></manifest>`
	if err := os.WriteFile(manifest, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	projects, skipped, err := LoadProjects(top, manifest, true)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(projects) != 1 {
		t.Fatalf("got %d skipped and %d projects", skipped, len(projects))
	}
	if projects[0].RelativePath != "a/project" || projects[0].Remote != "default" || projects[0].Expected != strings.Repeat("a", 40) {
		t.Fatalf("unexpected project: %+v", projects[0])
	}
}

func TestCommitRelation(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.name", "Test User")
	runGit(t, root, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(root, "base"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "base")
	base := runGit(t, root, "rev-parse", "HEAD")
	runGit(t, root, "checkout", "-b", "local")
	if err := os.WriteFile(filepath.Join(root, "local"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "local")
	local := runGit(t, root, "rev-parse", "HEAD")
	runGit(t, root, "checkout", "-b", "target", base)
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("target\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "target")
	target := runGit(t, root, "rev-parse", "HEAD")
	if got := CommitRelation(root, local, target); got != "diverged" {
		t.Fatalf("relation = %s, want diverged", got)
	}
}

func TestParseSelection(t *testing.T) {
	if got, _ := ParseSelection("1,3-4", 4); !equalInts(got, []int{0, 2, 3}) {
		t.Fatalf("selection = %v", got)
	}
	if got, _ := ParseSelection("a", 3); !equalInts(got, []int{0, 1, 2}) {
		t.Fatalf("all = %v", got)
	}
	if got, _ := ParseSelection("n", 3); len(got) != 0 {
		t.Fatalf("none = %v", got)
	}
	if _, err := ParseSelection("5", 3); err == nil {
		t.Fatal("out-of-range selection unexpectedly accepted")
	}
}

func TestManifestMayPrecedeOptions(t *testing.T) {
	options, _, err := parseOptions([]string{"manifest.xml", "--skip-pull", "-j", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Manifest != "manifest.xml" || !options.SkipPull || options.Jobs != 2 {
		t.Fatalf("unexpected options: %+v", options)
	}
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestGeneratedShellIsValid(t *testing.T) {
	root := t.TempDir()
	entry := SandboxEntry{
		Result: CheckResult{Project: Project{RelativePath: "repo", Expected: strings.Repeat("a", 40)}},
		Path:   filepath.Join(root, "worktrees", "repo"), RebaseBase: strings.Repeat("c", 40),
	}
	rc, err := WriteShellRC(root, []SandboxEntry{entry}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("bash", "-n", rc).CombinedOutput(); err != nil {
		t.Fatalf("generated shell is invalid: %v\n%s", err, output)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(data)
	if !strings.Contains(generated, "--onto") || !strings.Contains(generated, strings.Repeat("a", 40)) || !strings.Contains(generated, strings.Repeat("c", 40)) || !strings.Contains(generated, "HEAD") {
		t.Fatal("generated shell omitted explicit rebase source baseline")
	}
}

func TestGeneratedShellExposesManualRebaseContext(t *testing.T) {
	root := t.TempDir()
	entry := SandboxEntry{
		Result: CheckResult{Project: Project{RelativePath: "repo", Expected: strings.Repeat("a", 40)}, Actual: strings.Repeat("b", 40), ActualSubject: "local commit", ExpectedSubject: "target commit"},
		Path:   filepath.Join(root, "worktrees", "repo"), RebaseBase: strings.Repeat("c", 40), ManualRebase: true, ManualReason: "source baseline is not an ancestor of local HEAD",
	}
	rc, err := WriteShellRC(root, []SandboxEntry{entry}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("bash", "-n", rc).CombinedOutput(); err != nil {
		t.Fatalf("generated manual shell is invalid: %v\n%s", err, output)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(data)
	for _, expected := range []string{"uwu-info", "git --no-pager log --graph", "source baseline is not an ancestor", "Automatic rebase is disabled"} {
		if !strings.Contains(generated, expected) {
			t.Fatalf("generated shell omitted %q:\n%s", expected, generated)
		}
	}
	if strings.Contains(generated, "git rebase -i --onto 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' 'cccccccccccccccccccccccccccccccccccccccc' HEAD; return") {
		t.Fatal("manual sandbox unexpectedly contains an automatic rebase command")
	}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		command, err := shellStartupCommand(zsh, root, rc)
		if err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(zsh, "-n", filepath.Join(root, ".zshrc")).CombinedOutput(); err != nil {
			t.Fatalf("generated zsh startup file is invalid: %v\n%s", err, output)
		}
		if !strings.Contains(strings.Join(command.Env, "\n"), "ZDOTDIR="+root) {
			t.Fatal("zsh command did not preserve the temporary startup directory")
		}
	}
}

func TestReadLinePreservesBufferedInput(t *testing.T) {
	in := strings.NewReader("first\nsecond\n")
	var out bytes.Buffer
	if got := readLine(in, &out, ""); got != "first" {
		t.Fatalf("first line = %q", got)
	}
	if got := readLine(in, &out, ""); got != "second" {
		t.Fatalf("second line = %q", got)
	}
}
