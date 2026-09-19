// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

// Package release checks Android source repositories against SHA1-pinned repo
// manifests and provides the interactive development workflow used by uwu.
package release

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	sha1RE    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	versionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
)

const (
	ManifestDirectory  = "vendor/uwu-versions/manifest"
	ManifestRepository = "vendor/uwu-versions"
	lineagePrefix      = "LineageOS/"
	pixelOSPrefix      = "PixelOS-AOSP/"
)

type outputStyle struct {
	enabled   bool
	trueColor bool
	reset     string
	bold      string
	dim       string
	green     string
	red       string
	cyan      string
	yellow    string
	magenta   string
	brand     []string
}

var releaseStyle = detectOutputStyle(os.Stdout)

func detectOutputStyle(file *os.File) outputStyle {
	style := outputStyle{}
	if file == nil || !isTerminal(file) || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return style
	}
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor || strings.EqualFold(os.Getenv("UWU_NO_COLOR"), "true") {
		return style
	}
	style.enabled = true
	style.reset = "\033[0m"
	style.bold = "\033[1m"
	style.dim = "\033[2m"
	style.green = "\033[32m"
	style.red = "\033[31m"
	style.cyan = "\033[36m"
	style.yellow = "\033[33m"
	style.magenta = "\033[35m"
	if strings.Contains(os.Getenv("TERM"), "direct") || os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit" {
		style.trueColor = true
		style.brand = []string{
			"\033[38;2;141;227;253m",
			"\033[38;2;146;216;252m",
			"\033[38;2;151;206;251m",
			"\033[38;2;155;195;251m",
			"\033[38;2;166;203;252m",
			"\033[38;2;177;211;252m",
			"\033[38;2;188;219;253m",
		}
	}
	return style
}

func (style outputStyle) paint(code, value string) string {
	if !style.enabled || value == "" {
		return value
	}
	return code + value + style.reset
}

func (style outputStyle) boldText(value string) string { return style.paint(style.bold, value) }
func (style outputStyle) dimText(value string) string  { return style.paint(style.dim, value) }

func (style outputStyle) brandText(value string) string {
	if !style.enabled || !style.trueColor {
		return style.paint(style.cyan+style.bold, value)
	}
	var output strings.Builder
	for index, char := range value {
		if index < len(style.brand) {
			output.WriteString(style.brand[index])
			output.WriteString(style.bold)
			output.WriteRune(char)
			continue
		}
		output.WriteRune(char)
	}
	output.WriteString(style.reset)
	return output.String()
}

type Project struct {
	Path         string
	RelativePath string
	Expected     string
	Remote       string
	BaseRevision string
}

type CheckResult struct {
	Project           Project
	Status            string
	Actual            string
	Detail            string
	Dirty             bool
	ExpectedAvailable bool
	Relation          string
	ActualSubject     string
	ExpectedSubject   string
	LocalCommits      []string
	TargetCommits     []string
}

type SandboxEntry struct {
	Result         CheckResult
	Path           string
	OriginalHead   string
	OriginalBranch string
	OriginalStatus string
	RebaseBase     string
	ManualRebase   bool
	ManualReason   string
	SessionRoot    string
	DirtyPatch     string
	UntrackedFiles []string
}

type Options struct {
	Manifest       string
	Version        string
	FromVersion    string
	Jobs           int
	NoDirty        bool
	SkipPull       bool
	NoPrompt       bool
	ForceFullCheck bool
	Language       string
}

type manifestXML struct {
	Default  *defaultXML  `xml:"default"`
	Projects []projectXML `xml:"project"`
}

type defaultXML struct {
	Remote string `xml:"remote,attr"`
}

type projectXML struct {
	Name     string `xml:"name,attr"`
	Path     string `xml:"path,attr"`
	Revision string `xml:"revision,attr"`
	Remote   string `xml:"remote,attr"`
}

// ParseSelection parses the 1-based selection syntax used by the interactive
// menus. It returns zero-based indexes.
func ParseSelection(value string, count int) ([]int, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == "n" || v == "none" || v == "q" || v == "quit" {
		return []int{}, nil
	}
	if v == "a" || v == "all" {
		indexes := make([]int, count)
		for i := range indexes {
			indexes[i] = i
		}
		return indexes, nil
	}
	selected := map[int]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "-") {
			parts := strings.SplitN(item, "-", 2)
			start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 != nil || err2 != nil || start < 1 || end < 1 {
				return nil, fmt.Errorf("invalid selection: %s", item)
			}
			if start > end {
				start, end = end, start
			}
			for i := start - 1; i < end; i++ {
				selected[i] = true
			}
			continue
		}
		index, err := strconv.Atoi(item)
		if err != nil || index < 1 {
			return nil, fmt.Errorf("invalid selection: %s", item)
		}
		selected[index-1] = true
	}
	indexes := make([]int, 0, len(selected))
	for index := range selected {
		if index < 0 || index >= count {
			return nil, errors.New("selection is outside the displayed range")
		}
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes, nil
}

// ManifestVersion returns the semantic version encoded by a manifest name.
func ManifestVersion(manifest string) ([3]int, bool) {
	match := versionRE.FindStringSubmatch(strings.TrimSuffix(filepath.Base(manifest), filepath.Ext(manifest)))
	if match == nil {
		return [3]int{}, false
	}
	var version [3]int
	for i := range version {
		version[i], _ = strconv.Atoi(match[i+1])
	}
	return version, true
}

// LoadProjects loads SHA1-pinned direct projects from a repo manifest.
func LoadProjects(top, manifest string, skipLineagePixelOS bool) ([]Project, int, error) {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read %s: %w", manifest, err)
	}
	var parsed manifestXML
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return nil, 0, fmt.Errorf("failed to parse %s: %w", manifest, err)
	}
	defaultRemote := "origin"
	if parsed.Default != nil && parsed.Default.Remote != "" {
		defaultRemote = parsed.Default.Remote
	}
	root, err := filepath.Abs(top)
	if err != nil {
		return nil, 0, err
	}
	projects := make([]Project, 0, len(parsed.Projects))
	skipped := 0
	for _, node := range parsed.Projects {
		if !sha1RE.MatchString(node.Revision) {
			continue
		}
		remote := node.Remote
		if remote == "" {
			remote = defaultRemote
		}
		if skipLineagePixelOS && (remote == "lineageos" || strings.HasPrefix(node.Name, lineagePrefix) || strings.HasPrefix(node.Name, pixelOSPrefix)) {
			skipped++
			continue
		}
		projectPath := node.Path
		if projectPath == "" {
			projectPath = node.Name
		}
		if projectPath == "" {
			return nil, skipped, errors.New("SHA1 project is missing both path and name")
		}
		relative := filepath.ToSlash(filepath.Clean(projectPath))
		if relative == ManifestRepository {
			continue
		}
		if relative == ".." || strings.HasPrefix(relative, "../") || filepath.IsAbs(projectPath) {
			return nil, skipped, fmt.Errorf("project path escapes source tree: %s", projectPath)
		}
		localPath := filepath.Join(root, filepath.FromSlash(relative))
		projects = append(projects, Project{
			Path: localPath, RelativePath: relative,
			Expected: strings.ToLower(node.Revision), Remote: remote,
		})
	}
	return projects, skipped, nil
}

func VersionManifest(top, version string) (string, error) {
	if !versionRE.MatchString(version) {
		return "", fmt.Errorf("invalid manifest version %q; expected MAJOR.MINOR.PATCH", version)
	}
	return filepath.Join(top, filepath.FromSlash(ManifestDirectory), version+".xml"), nil
}

func LatestManifest(top string) (string, error) {
	directory := filepath.Join(top, filepath.FromSlash(ManifestDirectory))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	var latest string
	var latestVersion [3]int
	found := false
	for _, entry := range entries {
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".xml" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		version, ok := ManifestVersion(path)
		if ok && (!found || versionGreater(version, latestVersion)) {
			latest, latestVersion, found = path, version, true
		}
	}
	if !found {
		return "", fmt.Errorf("no versioned manifests found in %s", directory)
	}
	return latest, nil
}

func PreviousManifest(top, target string) (string, bool, error) {
	targetVersion, ok := ManifestVersion(target)
	if !ok {
		return "", false, nil
	}
	directory := filepath.Join(top, filepath.FromSlash(ManifestDirectory))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", false, err
	}
	var previous string
	var previousVersion [3]int
	found := false
	for _, entry := range entries {
		version, valid := ManifestVersion(entry.Name())
		if valid && versionLess(version, targetVersion) && (!found || versionGreater(version, previousVersion)) {
			previous, previousVersion, found = filepath.Join(directory, entry.Name()), version, true
		}
	}
	return previous, found, nil
}

func versionLess(left, right [3]int) bool { return !versionGreater(left, right) && left != right }
func versionGreater(left, right [3]int) bool {
	for i := range left {
		if left[i] != right[i] {
			return left[i] > right[i]
		}
	}
	return false
}

func isNonMilestoneManifest(manifest string) bool {
	version, ok := ManifestVersion(manifest)
	return ok && version[2]%100 != 0
}

type commandResult struct {
	stdout string
	stderr string
	code   int
}

func git(path string, args ...string) commandResult {
	commandArgs := append([]string{"-C", path}, args...)
	command := exec.Command("git", commandArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code = exitError.ExitCode()
		} else {
			code = -1
		}
	}
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// CommitRelation classifies actual relative to expected as seen by Git.
func CommitRelation(path, actual, expected string) string {
	if actual == expected {
		return "match"
	}
	if git(path, "merge-base", "--is-ancestor", expected, actual).code == 0 {
		return "ahead"
	}
	if git(path, "merge-base", "--is-ancestor", actual, expected).code == 0 {
		return "behind"
	}
	return "diverged"
}

func commitDescription(path, revision string) string {
	result := git(path, "show", "-s", "--format=%an%x00%ad%x00%s", "--date=short", revision)
	if result.code != 0 {
		return "unavailable"
	}
	fields := strings.SplitN(strings.TrimSpace(result.stdout), "\x00", 3)
	if len(fields) != 3 {
		return strings.TrimSpace(result.stdout)
	}
	return fmt.Sprintf("%s [%s, %s]", fields[2], fields[0], fields[1])
}

func commitSubjects(path, revisionRange string) []string {
	result := git(path, "log", "--format=%s", "--no-decorate", "--no-renames", "--max-count=8", revisionRange)
	if result.code != 0 {
		return nil
	}
	var subjects []string
	for _, line := range strings.Split(result.stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			subjects = append(subjects, strings.TrimSpace(line))
		}
	}
	return subjects
}

func worktreeStatus(path string) string {
	result := git(path, "status", "--porcelain=v1", "--untracked-files=all")
	if result.code != 0 {
		return ""
	}
	return result.stdout
}

func checkProject(project Project, checkDirty bool) CheckResult {
	if info, err := os.Stat(project.Path); err != nil || !info.IsDir() {
		return CheckResult{Project: project, Status: "missing", Detail: "local directory not found"}
	}
	topLevel := git(project.Path, "rev-parse", "--show-toplevel")
	if topLevel.code != 0 {
		detail := strings.TrimSpace(topLevel.stderr)
		if detail == "" {
			detail = "not a Git repository"
		}
		return CheckResult{Project: project, Status: "error", Detail: detail}
	}
	resolvedTop, _ := filepath.Abs(strings.TrimSpace(topLevel.stdout))
	resolvedProject, _ := filepath.Abs(project.Path)
	if resolvedTop != resolvedProject {
		return CheckResult{Project: project, Status: "error", Detail: "Git repository root does not match project path"}
	}
	head := git(project.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if head.code != 0 {
		detail := strings.TrimSpace(head.stderr)
		if detail == "" {
			detail = "unable to resolve HEAD"
		}
		return CheckResult{Project: project, Status: "error", Detail: detail}
	}
	actual := strings.ToLower(strings.TrimSpace(head.stdout))
	available := git(project.Path, "cat-file", "-e", project.Expected+"^{commit}").code == 0
	dirty := checkDirty && worktreeStatus(project.Path) != ""
	if actual == project.Expected {
		return CheckResult{Project: project, Status: "match", Actual: actual, Detail: "HEAD matches manifest", Dirty: dirty, ExpectedAvailable: true, Relation: "match", ActualSubject: commitDescription(project.Path, actual)}
	}
	result := CheckResult{Project: project, Status: "mismatch", Actual: actual, Dirty: dirty, ExpectedAvailable: available, Relation: "unknown", ActualSubject: commitDescription(project.Path, actual)}
	if available {
		result.Relation = CommitRelation(project.Path, actual, project.Expected)
		result.ExpectedSubject = commitDescription(project.Path, project.Expected)
		result.LocalCommits = commitSubjects(project.Path, project.Expected+".."+actual)
		result.TargetCommits = commitSubjects(project.Path, actual+".."+project.Expected)
	}
	detail := map[string]string{"ahead": "local development is ahead of the manifest", "behind": "local HEAD is behind the manifest", "diverged": "local history diverges from the manifest", "unknown": "target commit is not available locally"}[result.Relation]
	result.Detail = detail + "; local: " + result.ActualSubject
	if available {
		result.Detail += "; target: " + result.ExpectedSubject
	}
	return result
}

func fetchExpected(project Project, revision string) bool {
	if git(project.Path, "cat-file", "-e", revision+"^{commit}").code == 0 {
		return true
	}
	remote := project.Remote
	if remote == "" {
		remote = "origin"
	}
	git(project.Path, "fetch", "--no-tags", remote, revision)
	return git(project.Path, "cat-file", "-e", revision+"^{commit}").code == 0
}

func checkProjects(projects []Project, jobs int, checkDirty bool, out, errOut io.Writer) []CheckResult {
	if jobs < 1 {
		jobs = 1
	}
	results := make([]CheckResult, len(projects))
	work := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < jobs; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				results[index] = checkProject(projects[index], checkDirty)
			}
		}()
	}
	for index := range projects {
		work <- index
	}
	close(work)
	wait.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Project.RelativePath < results[j].Project.RelativePath })
	for index, result := range results {
		printProgress(out, errOut, index+1, len(results), result)
	}
	return results
}

func printProgress(out, errOut io.Writer, index, total int, result CheckResult) {
	stream := out
	icon := releaseStyle.paint(releaseStyle.green, "[OK]")
	detail := "HEAD matches manifest"
	if result.Dirty {
		icon = releaseStyle.paint(releaseStyle.yellow, "[DIRTY]")
		detail += "; worktree dirty"
	}
	if result.Status == "mismatch" {
		relationColor := releaseStyle.yellow
		if result.Relation == "ahead" {
			relationColor = releaseStyle.magenta
		} else if result.Relation == "behind" || result.Relation == "unknown" {
			relationColor = releaseStyle.red
		}
		stream, icon, detail = errOut, releaseStyle.paint(relationColor, "["+strings.ToUpper(result.Relation)+"]"), result.Detail
		if result.Dirty {
			icon = releaseStyle.paint(releaseStyle.yellow, "[DIRTY]") + " " + icon
		}
	} else if result.Status == "missing" {
		stream, icon, detail = errOut, releaseStyle.paint(releaseStyle.yellow, "[!]"), result.Detail
	} else if result.Status == "error" {
		stream, icon, detail = errOut, releaseStyle.paint(releaseStyle.red, "[!]"), result.Detail
	}
	fmt.Fprintf(stream, "%s %s %s: %s\n", releaseStyle.paint(releaseStyle.bold, fmt.Sprintf("[%d/%d]", index, total)), icon, result.Project.RelativePath, detail)
}

func printReport(out io.Writer, language string, results []CheckResult) {
	count := func(status string) int {
		n := 0
		for _, result := range results {
			if result.Status == status {
				n++
			}
		}
		return n
	}
	tr := translator(language)
	failed := count("mismatch") + count("missing") + count("error")
	dirty := 0
	relations := map[string]int{}
	for _, result := range results {
		if result.Dirty {
			dirty++
		}
		relations[result.Relation]++
	}
	state := releaseStyle.paint(releaseStyle.green+releaseStyle.bold, "PASSED")
	if failed != 0 {
		state = releaseStyle.paint(releaseStyle.red+releaseStyle.bold, "FAILED")
	}
	fmt.Fprintf(out, "\n%s\n--------------------------\n", releaseStyle.paint(releaseStyle.bold, tr("Manifest SHA1 check report", "Manifest SHA1 检查报告")))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Result", "结果"), state)
	fmt.Fprintf(out, "  %-16s%d\n", tr("Discovered", "已发现"), len(results))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Matched", "匹配"), releaseStyle.paint(releaseStyle.green, strconv.Itoa(count("match"))))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Mismatched", "不匹配"), releaseStyle.paint(releaseStyle.red, strconv.Itoa(count("mismatch"))))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Missing", "缺失"), releaseStyle.paint(releaseStyle.yellow, strconv.Itoa(count("missing"))))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Errors", "错误"), releaseStyle.paint(releaseStyle.red, strconv.Itoa(count("error"))))
	fmt.Fprintf(out, "  %-16s%s\n", tr("Dirty", "有修改"), releaseStyle.paint(releaseStyle.yellow, strconv.Itoa(dirty)))
	if len(relations) > 0 {
		fmt.Fprintf(out, "  %-16s%s=%d %s=%d %s=%d %s=%d\n", tr("Relations", "关系"), "ahead", relations["ahead"], "behind", relations["behind"], "diverged", relations["diverged"], "unknown", relations["unknown"])
	}
}

type releaseTranslator func(string, string) string

func translator(language string) releaseTranslator {
	if language == "zh" {
		return func(_ string, chinese string) string { return chinese }
	}
	return func(english, _ string) string { return english }
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func readLine(in io.Reader, out io.Writer, prompt string) string {
	fmt.Fprint(out, prompt)
	var line strings.Builder
	var value [1]byte
	for {
		count, err := in.Read(value[:])
		if count > 0 {
			if value[0] == '\n' {
				break
			}
			line.WriteByte(value[0])
		}
		if err != nil {
			if line.Len() == 0 {
				return ""
			}
			break
		}
	}
	return strings.TrimSpace(line.String())
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// WriteShellRC writes the interactive sandbox shell. The explicit source
// baseline is intentionally present in --onto: target source-base HEAD.
func WriteShellRC(sessionRoot string, entries []SandboxEntry, language string) (string, error) {
	path := filepath.Join(sessionRoot, "shell.rc")
	tr := translator(language)
	var lines []string
	lines = append(lines,
		"uwu_enter() {",
		"  if [ -z \"$1\" ]; then echo "+shellQuote(tr("Usage: uwu-enter <repository>", "用法：uwu-enter <仓库>"))+"; return 2; fi",
		"  cd -- "+shellQuote(filepath.Join(sessionRoot, "worktrees"))+"/\"$1\"",
		"}", "alias uwu-enter=uwu_enter", "uwu_status() {")
	for _, entry := range entries {
		lines = append(lines, "  echo "+shellQuote("--- "+entry.Result.Project.RelativePath), "  git -C "+shellQuote(entry.Path)+" status --short --branch")
	}
	lines = append(lines, "}", "alias uwu-status=uwu_status", "uwu_info() {", "  local root=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "+shellQuote(tr("Not inside a managed repository", "当前不在受管理的仓库内"))+"; return 1; }", "  case \"$root\" in")
	for _, entry := range entries {
		base := entry.RebaseBase
		if base == "" {
			base = "<not configured>"
		}
		lines = append(lines,
			"    "+shellQuote(entry.Path)+")",
			"      echo "+shellQuote("Repository: "+entry.Result.Project.RelativePath),
			"      echo "+shellQuote("Local HEAD: "+entry.OriginalHead),
			"      echo "+shellQuote("Manifest target: "+entry.Result.Project.Expected),
			"      echo "+shellQuote("Source baseline: "+base),
			"      echo "+shellQuote("Local commit: "+entry.Result.ActualSubject),
			"      echo "+shellQuote("Target commit: "+entry.Result.ExpectedSubject),
			"      git status --short --branch",
			"      git --no-pager branch --all --verbose --no-abbrev",
			"      git remote --verbose",
			"      git --no-pager log --graph --oneline --decorate --all -20",
		)
		if entry.RebaseBase != "" {
			lines = append(lines,
				"      echo "+shellQuote("Manifest baseline relation:"),
				"      git merge-base "+shellQuote(entry.RebaseBase)+" HEAD || true",
				"      echo "+shellQuote("Commits on both sides of the proposed baseline:"),
				"      git --no-pager log --left-right --cherry-pick --oneline "+shellQuote(entry.RebaseBase)+"...HEAD || true",
			)
		}
		if entry.ManualRebase {
			lines = append(lines,
				"      echo "+shellQuote("Automatic rebase disabled: "+entry.ManualReason),
				"      echo "+shellQuote(tr("Choose a real local ancestor, then run: git rebase -i --onto <manifest-target> <chosen-ancestor> HEAD", "请选择当前 HEAD 的真实祖先，然后执行：git rebase -i --onto <manifest-target> <chosen-ancestor> HEAD")),
			)
		} else {
			lines = append(lines, "      echo "+shellQuote("Automatic command: git rebase -i --onto "+entry.Result.Project.Expected+" "+entry.RebaseBase+" HEAD"))
		}
		lines = append(lines, "      return 0;;")
	}
	lines = append(lines, "    *) echo "+shellQuote(tr("Not a managed temporary repository", "当前目录不是受管理的临时仓库"))+"; return 2;;", "  esac", "}", "alias uwu-info=uwu_info", "uwu_rebase() {", "  local root=$(git rev-parse --show-toplevel 2>/dev/null) || return 1", "  case \"$root\" in")
	for _, entry := range entries {
		lines = append(lines,
			"    "+shellQuote(entry.Path)+")",
		)
		if entry.ManualRebase {
			lines = append(lines,
				"      echo "+shellQuote(tr("Automatic rebase is disabled for this repository. Run uwu-info and choose the baseline yourself.", "此仓库已禁用自动 rebase。请运行 uwu-info 并自行选择基线。")),
				"      return 2;;",
			)
		} else {
			lines = append(lines, "      git rebase -i --onto "+shellQuote(entry.Result.Project.Expected)+" "+shellQuote(entry.RebaseBase)+" HEAD; return $?;;")
		}
	}
	lines = append(lines, "    *) echo "+shellQuote(tr("Not a managed temporary repository", "当前目录不是受管理的临时仓库"))+"; return 2;;", "  esac", "}", "alias uwu-rebase=uwu_rebase",
		"echo "+shellQuote(tr("Temporary rebase shell. Main repositories are unchanged.", "临时 rebase shell，主仓库尚未修改。")),
		"echo "+shellQuote(tr("Commands: uwu-status, uwu-enter <path>, uwu-info, uwu-rebase", "命令：uwu-status、uwu-enter <路径>、uwu-info、uwu-rebase")),
		"echo "+shellQuote(tr("Run uwu-info inside a repository before choosing a manual rebase base.", "进入仓库后先运行 uwu-info，再选择手动 rebase 基线。")))
	if len(entries) == 1 {
		lines = append(lines, "cd -- "+shellQuote(entries[0].Path), "echo "+shellQuote(tr("Selected repository is ready. Run uwu-info, then uwu-rebase or a manual git command.", "已进入选中的仓库，请先运行 uwu-info，再执行 uwu-rebase 或手动 git 命令。")))
	} else {
		lines = append(lines, "uwu-status")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func shellStartupCommand(shellPath, sessionRoot, shellRC string) (*exec.Cmd, error) {
	shellName := filepath.Base(shellPath)
	command := exec.Command(shellPath, "-i")
	switch shellName {
	case "bash":
		// --rcfile keeps the helper functions scoped to this temporary shell.
		// The generated file sources the user's normal .bashrc first.
		command = exec.Command(shellPath, "--rcfile", shellRC, "-i")
	case "zsh":
		originalZdotDir := os.Getenv("ZDOTDIR")
		if originalZdotDir == "" {
			originalZdotDir = os.Getenv("HOME")
		}
		zshRC := filepath.Join(sessionRoot, ".zshrc")
		content := strings.Join([]string{
			"if [[ -z \"${UWU_RELEASE_USER_RC_LOADED:-}\" ]]; then",
			"  export UWU_RELEASE_USER_RC_LOADED=1",
			"  if [[ -f " + shellQuote(filepath.Join(originalZdotDir, ".zshrc")) + " ]]; then",
			"    source " + shellQuote(filepath.Join(originalZdotDir, ".zshrc")),
			"  fi",
			"fi",
			"source " + shellQuote(shellRC),
		}, "\n") + "\n"
		if err := os.WriteFile(zshRC, []byte(content), 0600); err != nil {
			return nil, err
		}
		command.Env = append(os.Environ(), "ZDOTDIR="+sessionRoot)
	case "fish":
		// Fish has a different function syntax; keep the user's startup files
		// and ask it to source the POSIX helper only when explicitly supported.
		return nil, fmt.Errorf("fish shells are not supported for helper injection; source %s manually", shellRC)
	default:
		return nil, fmt.Errorf("unsupported interactive shell %q; source %s manually", shellPath, shellRC)
	}
	return command, nil
}

func captureChanges(project Project, sessionRoot string) (string, []string) {
	patchResult := git(project.Path, "diff", "--binary", "HEAD")
	var patch string
	if patchResult.code == 0 && patchResult.stdout != "" {
		patch = filepath.Join(sessionRoot, "patches", strings.ReplaceAll(project.RelativePath, "/", "__")+".patch")
		os.MkdirAll(filepath.Dir(patch), 0700)
		os.WriteFile(patch, []byte(patchResult.stdout), 0600)
	}
	list := git(project.Path, "ls-files", "--others", "--exclude-standard", "-z")
	var untracked []string
	if list.code == 0 {
		for _, relative := range strings.Split(list.stdout, "\x00") {
			if relative == "" {
				continue
			}
			source := filepath.Join(project.Path, filepath.FromSlash(relative))
			destination := filepath.Join(sessionRoot, "untracked", project.RelativePath, relative)
			if info, err := os.Lstat(source); err == nil {
				os.MkdirAll(filepath.Dir(destination), 0700)
				if info.Mode()&os.ModeSymlink != 0 {
					target, _ := os.Readlink(source)
					os.Symlink(target, destination)
				} else if info.Mode().IsRegular() {
					copyFile(source, destination)
				}
				untracked = append(untracked, relative)
			}
		}
	}
	return patch, untracked
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func restoreChanges(entry SandboxEntry) bool {
	if entry.DirtyPatch != "" {
		result := git(entry.Result.Project.Path, "apply", "--binary", "--3way", entry.DirtyPatch)
		if result.code != 0 {
			return false
		}
	}
	for _, relative := range entry.UntrackedFiles {
		source := filepath.Join(entry.SessionRoot, "untracked", entry.Result.Project.RelativePath, relative)
		destination := filepath.Join(entry.Result.Project.Path, relative)
		if _, err := os.Lstat(destination); err == nil {
			continue
		}
		if info, err := os.Lstat(source); err == nil {
			os.MkdirAll(filepath.Dir(destination), 0700)
			if info.Mode()&os.ModeSymlink != 0 {
				target, _ := os.Readlink(source)
				os.Symlink(target, destination)
			} else {
				if copyFile(source, destination) != nil {
					return false
				}
			}
		}
	}
	return true
}

func prepareSandboxes(selected []CheckResult, sessionRoot string, out, errOut io.Writer) []SandboxEntry {
	var entries []SandboxEntry
	for _, result := range selected {
		project := result.Project
		manualRebase := false
		manualReason := ""
		if !fetchExpected(project, project.Expected) {
			manualRebase = true
			manualReason = "target commit is unavailable locally"
			fmt.Fprintf(errOut, "Cannot resolve target for %s automatically; keeping an inspection sandbox\n", project.RelativePath)
		}
		if project.BaseRevision == "" {
			manualRebase = true
			manualReason = "no source manifest baseline is configured"
		} else if !fetchExpected(project, project.BaseRevision) {
			manualRebase = true
			manualReason = "source baseline is unavailable locally"
		} else if git(project.Path, "merge-base", "--is-ancestor", project.BaseRevision, result.Actual).code != 0 {
			manualRebase = true
			manualReason = "source baseline is not an ancestor of local HEAD"
		}
		branchResult := git(project.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
		branch := ""
		if branchResult.code == 0 {
			branch = strings.TrimSpace(branchResult.stdout)
		}
		path := filepath.Join(sessionRoot, "worktrees", filepath.FromSlash(project.RelativePath))
		os.MkdirAll(filepath.Dir(path), 0700)
		patch, untracked := captureChanges(project, sessionRoot)
		add := git(project.Path, "worktree", "add", "--detach", path, result.Actual)
		if add.code != 0 {
			fmt.Fprintf(errOut, "Cannot prepare %s: %s\n", project.RelativePath, strings.TrimSpace(add.stderr))
			continue
		}
		entries = append(entries, SandboxEntry{Result: result, Path: path, OriginalHead: result.Actual, OriginalBranch: branch, OriginalStatus: worktreeStatus(project.Path), RebaseBase: project.BaseRevision, ManualRebase: manualRebase, ManualReason: manualReason, SessionRoot: sessionRoot, DirtyPatch: patch, UntrackedFiles: untracked})
		if manualRebase {
			fmt.Fprintf(out, "Prepared manual rebase sandbox: %s (%s)\n", project.RelativePath, manualReason)
		} else {
			fmt.Fprintf(out, "Prepared temporary repository: %s\n", project.RelativePath)
		}
	}
	return entries
}

func cleanupSandboxes(entries []SandboxEntry, sessionRoot string) {
	for _, entry := range entries {
		git(entry.Result.Project.Path, "worktree", "remove", "--force", entry.Path)
	}
	os.RemoveAll(sessionRoot)
}

func applySandboxes(entries []SandboxEntry, out, errOut io.Writer, language string) bool {
	tr := translator(language)
	ok := true
	for _, entry := range entries {
		project := entry.Result.Project
		currentHead := git(project.Path, "rev-parse", "--verify", "HEAD^{commit}")
		if currentHead.code != 0 || strings.TrimSpace(currentHead.stdout) != entry.OriginalHead {
			fmt.Fprintf(errOut, "%s %s: %s\n", tr("Skipped", "已跳过"), project.RelativePath, tr("main HEAD changed", "主仓库 HEAD 已变化"))
			ok = false
			continue
		}
		branch := git(project.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
		currentBranch := ""
		if branch.code == 0 {
			currentBranch = strings.TrimSpace(branch.stdout)
		}
		if currentBranch != entry.OriginalBranch || worktreeStatus(project.Path) != entry.OriginalStatus {
			fmt.Fprintf(errOut, "%s %s: %s\n", tr("Skipped", "已跳过"), project.RelativePath, tr("main branch or worktree changed", "主仓库分支或工作区已变化"))
			ok = false
			continue
		}
		if worktreeStatus(entry.Path) != "" {
			fmt.Fprintf(errOut, "%s %s: %s\n", tr("Skipped", "已跳过"), project.RelativePath, tr("sandbox is not clean", "临时工作区不干净"))
			ok = false
			continue
		}
		sandboxHead := git(entry.Path, "rev-parse", "--verify", "HEAD^{commit}")
		if sandboxHead.code != 0 {
			ok = false
			continue
		}
		reset := git(project.Path, "reset", "--hard", strings.TrimSpace(sandboxHead.stdout))
		if reset.code != 0 || !restoreChanges(entry) {
			fmt.Fprintf(errOut, "Failed to apply %s\n", project.RelativePath)
			ok = false
			continue
		}
		fmt.Fprintf(out, "%s %s\n", tr("Applied", "已应用"), project.RelativePath)
	}
	return ok
}

func chooseResults(in io.Reader, out io.Writer, results []CheckResult, title, language string) []CheckResult {
	if len(results) == 0 {
		return nil
	}
	tr := translator(language)
	for {
		fmt.Fprintf(out, "\n%s\n", title)
		fmt.Fprintln(out, tr("Enter numbers, ranges such as 1-3, 'a' for all, or 'n' for none.", "输入编号、范围（如 1-3）、a 表示全部，n 表示不选择。"))
		for index, result := range results {
			fmt.Fprintf(out, "  %2d) %-8s %s: %s\n", index+1, strings.ToUpper(result.Relation), result.Project.RelativePath, result.ActualSubject)
		}
		value := readLine(in, out, tr("Select repositories: ", "选择仓库："))
		indexes, err := ParseSelection(value, len(results))
		if err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		selected := make([]CheckResult, 0, len(indexes))
		for _, index := range indexes {
			selected = append(selected, results[index])
		}
		return selected
	}
}

func chooseBase(top, target, requested, language string, in io.Reader, out io.Writer) (string, error) {
	tr := translator(language)
	if requested != "" {
		return VersionManifest(top, requested)
	}
	defaultBase, found, err := PreviousManifest(top, target)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New(tr("no previous versioned manifest is available as a rebase base", "没有可作为 rebase 基线的上一版本 manifest"))
	}
	defaultVersion := strings.TrimSuffix(filepath.Base(defaultBase), filepath.Ext(defaultBase))
	fmt.Fprintf(out, "%s\n", tr("Rebase source baseline", "Rebase 源 manifest"))
	fmt.Fprintf(out, tr("The default source baseline is %s. Press Enter to use it, or enter another version.\n", "默认源基线为 %s。直接回车使用它，或输入其他版本号。\n"), defaultVersion)
	value := readLine(in, out, tr("Source version: ", "源版本："))
	if value == "" {
		return defaultBase, nil
	}
	return VersionManifest(top, value)
}

func runRebaseShell(selected []CheckResult, top, language string, in io.Reader, out, errOut io.Writer) bool {
	if len(selected) == 0 {
		return true
	}
	sessionRoot, err := os.MkdirTemp("", "uwu-rebase-")
	if err != nil {
		return false
	}
	entries := prepareSandboxes(selected, sessionRoot, out, errOut)
	if len(entries) == 0 {
		os.RemoveAll(sessionRoot)
		return false
	}
	rc, err := WriteShellRC(sessionRoot, entries, language)
	if err != nil {
		cleanupSandboxes(entries, sessionRoot)
		return false
	}
	shellPath := os.Getenv("SHELL")
	if shellPath == "" {
		shellPath = "bash"
	}
	shellPath, err = exec.LookPath(shellPath)
	if err != nil {
		cleanupSandboxes(entries, sessionRoot)
		return false
	}
	for {
		command, err := shellStartupCommand(shellPath, sessionRoot, rc)
		if err != nil {
			fmt.Fprintln(errOut, err)
			cleanupSandboxes(entries, sessionRoot)
			return false
		}
		command.Dir, command.Stdin, command.Stdout, command.Stderr = sessionRoot, os.Stdin, os.Stdout, os.Stderr
		_ = command.Run()
		fmt.Fprintln(out, "\nTemporary rebase results")
		for _, entry := range entries {
			state := "ready"
			if worktreeStatus(entry.Path) != "" {
				state = "dirty or rebase in progress"
			}
			fmt.Fprintf(out, "  %s: %s\n", entry.Result.Project.RelativePath, state)
		}
		fmt.Fprintln(out, "  1) "+translator(language)("Apply sandbox results", "应用临时工作区结果"))
		fmt.Fprintln(out, "  2) "+translator(language)("Reopen shell", "重新打开 shell"))
		fmt.Fprintln(out, "  3) "+translator(language)("Discard sandbox", "放弃临时工作区"))
		choice := readLine(in, out, translator(language)("Choose [1-3]: ", "请选择 [1-3]："))
		switch choice {
		case "1":
			applied := applySandboxes(entries, out, errOut, language)
			if applied {
				cleanupSandboxes(entries, sessionRoot)
			} else {
				fmt.Fprintf(out, "%s: %s\n", translator(language)("Sandbox kept for review", "临时工作区已保留供检查"), sessionRoot)
			}
			return applied
		case "3":
			cleanupSandboxes(entries, sessionRoot)
			return false
		}
	}
}

func overwriteCandidates(results []CheckResult) []CheckResult {
	var candidates []CheckResult
	for _, result := range results {
		if result.Status == "missing" || result.Status == "mismatch" || (result.Status == "match" && result.Dirty) {
			candidates = append(candidates, result)
		}
	}
	return candidates
}

func syncMissingProjects(top string, selected []CheckResult, out, errOut io.Writer) map[string]CheckResult {
	missing := make([]CheckResult, 0)
	paths := make([]string, 0)
	for _, result := range selected {
		if result.Status == "missing" {
			missing = append(missing, result)
			paths = append(paths, result.Project.RelativePath)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	updated := make(map[string]CheckResult, len(missing))
	repo, err := exec.LookPath("repo")
	if err != nil {
		for _, result := range missing {
			result.Status = "error"
			result.Detail = "repo command was not found"
			updated[result.Project.RelativePath] = result
		}
		return updated
	}
	fmt.Fprintf(out, "[info] Syncing %d missing repository(ies) with repo...\n", len(missing))
	args := []string{"sync", "--no-tags", "--current-branch", "--optimized-fetch", "--no-clone-bundle", "--force-sync", "--force-checkout"}
	args = append(args, paths...)
	command := exec.Command(repo, args...)
	command.Dir, command.Stdout, command.Stderr = top, os.Stdout, os.Stderr
	syncErr := command.Run()
	for _, result := range missing {
		verified := checkProject(result.Project, true)
		if syncErr != nil && verified.Status != "match" {
			verified.Status = "error"
			verified.Detail = fmt.Sprintf("repo sync failed: %v", syncErr)
		}
		updated[result.Project.RelativePath] = verified
	}
	if syncErr != nil {
		fmt.Fprintln(errOut, "repo sync failed:", syncErr)
	}
	return updated
}

func overwriteProjects(top string, selected []CheckResult, out, errOut io.Writer) []CheckResult {
	synced := syncMissingProjects(top, selected, out, errOut)
	updated := make([]CheckResult, 0, len(selected))
	for _, result := range selected {
		if result.Status == "missing" {
			if replacement, ok := synced[result.Project.RelativePath]; ok {
				updated = append(updated, replacement)
			} else {
				fmt.Fprintf(errOut, "Cannot overwrite missing repository %s\n", result.Project.RelativePath)
				updated = append(updated, result)
			}
			continue
		}
		target := "HEAD"
		if result.Status == "mismatch" {
			target = result.Project.Expected
			if !fetchExpected(result.Project, target) {
				fmt.Fprintf(errOut, "Cannot fetch target for %s\n", result.Project.RelativePath)
				updated = append(updated, result)
				continue
			}
		}
		if git(result.Project.Path, "reset", "--hard", target).code != 0 {
			fmt.Fprintf(errOut, "Cannot overwrite %s\n", result.Project.RelativePath)
			updated = append(updated, result)
			continue
		}
		updatedResult := checkProject(result.Project, true)
		updated = append(updated, updatedResult)
		fmt.Fprintf(out, "%s: %s\n", result.Project.RelativePath, updatedResult.Status)
	}
	return updated
}

func parseOptions(args []string) (Options, bool, error) {
	options := Options{Jobs: 4}
	help := false
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&help, "help", false, "show help")
	flags.BoolVar(&help, "h", false, "show help")
	flags.StringVar(&options.Version, "version", "", "manifest version")
	flags.StringVar(&options.FromVersion, "from-version", "", "source manifest version")
	flags.IntVar(&options.Jobs, "jobs", 4, "parallel repository checks")
	flags.IntVar(&options.Jobs, "j", 4, "parallel repository checks")
	flags.BoolVar(&options.NoDirty, "no-dirty", false, "do not check tracked worktree modifications")
	flags.BoolVar(&options.SkipPull, "skip-pull", false, "skip manifest repository update")
	flags.BoolVar(&options.NoPrompt, "no-prompt", false, "disable interactive prompts")
	flags.BoolVar(&options.ForceFullCheck, "force-full-check", false, "include all projects")
	flags.StringVar(&options.Language, "language", os.Getenv("UWU_LANG"), "language: en or zh")
	if err := flags.Parse(reorderPositionals(args)); err != nil {
		return options, false, err
	}
	if help {
		return options, true, nil
	}
	if options.Jobs < 1 {
		return options, false, errors.New("--jobs must be at least 1")
	}
	if options.Language != "" && options.Language != "en" && options.Language != "zh" {
		return options, false, errors.New("--language must be en or zh")
	}
	positionals := flags.Args()
	if len(positionals) > 1 {
		return options, false, errors.New("only one manifest path may be specified")
	}
	if len(positionals) == 1 {
		options.Manifest = positionals[0]
	}
	if options.Manifest != "" && options.Version != "" {
		return options, false, errors.New("manifest path and --version cannot be used together")
	}
	return options, false, nil
}

// The Python argparse command accepts a manifest before or after options.
// The standard flag package stops parsing at the first positional, so move
// the single positional to the end while preserving option/value pairs.
func reorderPositionals(args []string) []string {
	valueFlags := map[string]bool{"--version": true, "--from-version": true, "--jobs": true, "-j": true, "--language": true}
	options := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	waitingForValue := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if waitingForValue {
			options = append(options, arg)
			waitingForValue = false
			continue
		}
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		options = append(options, arg)
		name := arg
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			name = name[:equals]
		}
		if valueFlags[name] && !strings.Contains(arg, "=") {
			waitingForValue = true
		}
	}
	return append(options, positionals...)
}

func findTop(manifest string) (string, error) {
	path := manifest
	if path == "" {
		path = ManifestDirectory
	}
	var candidates []string
	if filepath.IsAbs(path) {
		candidates = []string{path}
	} else {
		current, _ := os.Getwd()
		for directory := current; ; directory = filepath.Dir(directory) {
			candidate, _ := filepath.Abs(filepath.Join(directory, path))
			candidates = append(candidates, candidate)
			if filepath.Dir(directory) == directory {
				break
			}
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		start := candidate
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			start = filepath.Dir(candidate)
		}
		for top := start; ; top = filepath.Dir(top) {
			if isDirectory(filepath.Join(top, ".repo")) && isFile(filepath.Join(top, "build/envsetup.sh")) {
				return top, nil
			}
			if filepath.Dir(top) == top {
				break
			}
		}
		return "", fmt.Errorf("path is outside an Android source tree: %s", candidate)
	}
	return "", fmt.Errorf("manifest not found: %s", manifest)
}

func isDirectory(path string) bool { info, err := os.Stat(path); return err == nil && info.IsDir() }
func isFile(path string) bool      { info, err := os.Stat(path); return err == nil && !info.IsDir() }

func printBanner(out io.Writer, top, language string) {
	if language == "zh" {
		fmt.Fprintf(out, "\n%s\n%s\n%s: %s\n\n",
			releaseStyle.brandText("uwuAOSP Manifest SHA1 检查器"),
			releaseStyle.dimText("检查本地仓库是否匹配 manifest 固定的 SHA1"),
			releaseStyle.dimText("源码树"), filepath.Base(top))
		return
	}
	fmt.Fprintf(out, "\n%s\n%s\n%s: %s\n\n",
		releaseStyle.brandText("uwuAOSP Manifest SHA1 Checker"),
		releaseStyle.dimText("Check local repositories against manifest-pinned SHA1 revisions"),
		releaseStyle.dimText("Source"), filepath.Base(top))
}

func resolveManifest(top, manifest string) string {
	if filepath.IsAbs(manifest) {
		return filepath.Clean(manifest)
	}
	if current, err := filepath.Abs(manifest); err == nil && isFile(current) {
		return current
	}
	return filepath.Join(top, manifest)
}

func pullManifestRepo(top string) error {
	repository := filepath.Join(top, filepath.FromSlash(ManifestRepository))
	if !isDirectory(repository) {
		return fmt.Errorf("manifest repository not found: %s", repository)
	}
	command := exec.Command("git", "-C", repository, "pull")
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git pull failed for %s: %w", repository, err)
	}
	return nil
}

func selectBaseProjects(top, baseline string, projects []Project, skip bool) ([]Project, error) {
	baseProjects, _, err := LoadProjects(top, baseline, skip)
	if err != nil {
		return nil, err
	}
	base := map[string]string{}
	for _, project := range baseProjects {
		base[project.RelativePath] = project.Expected
	}
	for index := range projects {
		projects[index].BaseRevision = base[projects[index].RelativePath]
	}
	return projects, nil
}

func chooseDevelopment(results []CheckResult, language string, in io.Reader, out io.Writer) bool {
	var mismatches []CheckResult
	for _, result := range results {
		if result.Status == "mismatch" {
			mismatches = append(mismatches, result)
		}
	}
	if len(mismatches) == 0 {
		return false
	}
	tr := translator(language)
	fmt.Fprintln(out, "\nDevelopment mode")
	fmt.Fprintln(out, tr("The local tree differs from the release manifest. Is this intentional work for the next version?", "本地源码树与发布 manifest 不一致。这是否属于下一版本的开发内容？"))
	fmt.Fprintln(out, tr("  1) Yes, keep local-ahead work and review behind/diverged repositories", "  1) 是，保留本地领先内容，并检查落后或分叉仓库"))
	fmt.Fprintln(out, tr("  2) No, use strict release comparison", "  2) 否，使用严格版本检查"))
	fmt.Fprintln(out, tr("  3) Cancel", "  3) 取消"))
	return readLine(in, out, tr("Choose [1-3]: ", "请选择 [1-3]：")) == "1"
}

func chooseOverwrites(results []CheckResult, language string, in io.Reader, out io.Writer) []CheckResult {
	candidates := overwriteCandidates(results)
	if len(candidates) == 0 {
		return nil
	}
	tr := translator(language)
	fmt.Fprintln(out, "\nStrict overwrite mode")
	fmt.Fprintln(out, tr("Tracked changes will be discarded only after an explicit confirmation.", "只有明确确认后才会丢弃已跟踪文件的修改。"))
	if readLine(in, out, tr("Type 'select' to choose repositories, or anything else to cancel: ", "输入 select 选择仓库，输入其他内容取消：")) != "select" {
		return nil
	}
	return chooseResults(in, out, candidates, tr("Select repositories to overwrite", "选择要覆盖的仓库"), language)
}

// Run executes the release checker and returns a process-style exit status.
func Run(args []string) int {
	options, help, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		return 2
	}
	if help {
		fmt.Println("Usage: release [manifest] [options]")
		fmt.Println("  --version VERSION         Check a versioned manifest")
		fmt.Println("  --from-version VERSION   Source version for interactive rebases")
		fmt.Println("  -j, --jobs JOBS           Number of parallel checks (default 4)")
		fmt.Println("  --no-dirty                Do not check worktree modifications")
		fmt.Println("  --skip-pull               Do not update the manifest repository")
		fmt.Println("  --no-prompt               Disable interactive operations")
		fmt.Println("  --force-full-check        Include LineageOS/PixelOS in non-milestones")
		fmt.Println("  --language en|zh          Select output language")
		return 0
	}
	releaseStyle = detectOutputStyle(os.Stdout)
	language := options.Language
	tr := translator(language)
	requested := options.Manifest
	if requested == "" {
		requested = ManifestDirectory
	}
	top, err := findTop(requested)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		return 2
	}
	printBanner(os.Stdout, top, language)
	if options.SkipPull {
		fmt.Fprintln(os.Stdout, "[info] Skipping manifest repository update")
	} else if err := pullManifestRepo(top); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		return 2
	}
	var manifest string
	if options.Version != "" {
		manifest, err = VersionManifest(top, options.Version)
	} else if options.Manifest != "" {
		manifest = resolveManifest(top, options.Manifest)
	} else {
		manifest, err = LatestManifest(top)
	}
	if err == nil && !isFile(manifest) {
		err = fmt.Errorf("manifest not found: %s", manifest)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		return 2
	}
	skip := !options.ForceFullCheck && isNonMilestoneManifest(manifest)
	projects, skipped, err := LoadProjects(top, manifest, skip)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		return 2
	}
	if skipped != 0 {
		fmt.Fprintf(os.Stdout, "[info] Skipping %d LineageOS/PixelOS project(s) for this non-milestone manifest; use --force-full-check to include them\n", skipped)
	}
	relativeManifest, _ := filepath.Rel(top, manifest)
	fmt.Fprintf(os.Stdout, "[info] Manifest: %s\n", filepath.ToSlash(relativeManifest))
	fmt.Fprintf(os.Stdout, "[info] Checking %d SHA1-pinned repository(ies) with %d worker(s)...\n", len(projects), options.Jobs)
	results := checkProjects(projects, options.Jobs, !options.NoDirty, os.Stdout, os.Stderr)
	printReport(os.Stdout, language, results)

	development := false
	input := os.Stdin
	if !options.NoPrompt && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		development = chooseDevelopment(results, language, input, os.Stdout)
	}
	if development {
		baseline, baseErr := chooseBase(top, manifest, options.FromVersion, language, input, os.Stdout)
		if baseErr != nil {
			fmt.Fprintln(os.Stderr, baseErr)
			return 1
		}
		targetVersion, targetOK := ManifestVersion(manifest)
		baseVersion, baseOK := ManifestVersion(baseline)
		if targetOK && baseOK && !versionLess(baseVersion, targetVersion) {
			fmt.Fprintln(os.Stderr, tr("Source baseline must be older than the target manifest.", "源基线必须早于目标 manifest。"))
			return 1
		}
		projects, err = selectBaseProjects(top, baseline, projects, skip)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		byPath := map[string]Project{}
		for _, project := range projects {
			byPath[project.RelativePath] = project
		}
		for index := range results {
			results[index].Project = byPath[results[index].Project.RelativePath]
		}
		var candidates []CheckResult
		for _, result := range results {
			if result.Status == "mismatch" && (result.Relation == "behind" || result.Relation == "diverged" || result.Relation == "unknown") {
				candidates = append(candidates, result)
			}
		}
		selected := chooseResults(input, os.Stdout, candidates, "Select repositories to rebase in a temporary shell", language)
		if len(selected) > 0 && !runRebaseShell(selected, top, language, input, os.Stdout, os.Stderr) {
			return 1
		}
		fmt.Fprintln(os.Stdout, tr("Development session complete", "开发会话完成"))
		return 0
	}
	if !options.NoPrompt && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		selected := chooseOverwrites(results, language, input, os.Stdout)
		if len(selected) > 0 && readLine(input, os.Stdout, tr("Type 'discard' to proceed, or anything else to cancel: ", "输入 discard 确认继续，输入其他内容取消：")) == "discard" {
			updated := overwriteProjects(top, selected, os.Stdout, os.Stderr)
			byPath := map[string]CheckResult{}
			for _, result := range updated {
				byPath[result.Project.RelativePath] = result
			}
			for index, result := range results {
				if replacement, ok := byPath[result.Project.RelativePath]; ok {
					results[index] = replacement
				}
			}
			printReport(os.Stdout, language, results)
		}
	}
	for _, result := range results {
		if result.Status != "match" {
			return 1
		}
	}
	return 0
}
