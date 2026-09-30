// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

// Package repo provides uwuCLI's repo synchronization command.
package repo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const excludedRemote = "uwuAOSP"

type manifest struct {
	Default  manifestDefault   `json:"default"`
	Projects []manifestProject `json:"project"`
}

type manifestDefault struct {
	Remote string `json:"remote"`
}

type manifestProject struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Remote string `json:"remote"`
}

type project struct {
	Path   string
	Remote string
}

type options struct {
	IncludeUwuaosp bool
	Help           bool
	Args           []string
}

// Run executes repo sync in top, filtering out uwuAOSP projects unless the
// caller explicitly requests them.
func Run(args []string, out, errOut io.Writer) error {
	parsed, err := parseOptions(args)
	if err != nil {
		return err
	}
	if parsed.Help {
		printUsage(out)
		return nil
	}

	top, err := findSourceRoot()
	if err != nil {
		return err
	}
	repoPath, err := exec.LookPath("repo")
	if err != nil {
		return errors.New("repo command was not found")
	}

	projects, err := loadProjects(top, repoPath)
	if err != nil {
		return err
	}
	selected, syncArgs, err := selectProjects(top, projects, parsed.Args, parsed.IncludeUwuaosp)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return errors.New("no repo projects remain after excluding uwuAOSP")
	}

	commandArgs := buildSyncArgs(syncArgs, selected)
	command := exec.Command(repoPath, commandArgs...)
	command.Dir = top
	command.Stdout = out
	command.Stderr = errOut
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return fmt.Errorf("repo sync exited with status %d", exitError.ExitCode())
		}
		return fmt.Errorf("run repo sync: %w", err)
	}
	return nil
}

func parseOptions(args []string) (options, error) {
	parsed := options{Args: make([]string, 0, len(args))}
	for _, arg := range args {
		switch arg {
		case "--include-uwuaosp":
			parsed.IncludeUwuaosp = true
		case "-h", "--help":
			parsed.Help = true
		default:
			parsed.Args = append(parsed.Args, arg)
		}
	}
	return parsed, nil
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage: uwu repo sync [SELECTORS...] [REPO SYNC OPTIONS...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Sync all manifest projects except remote uwuAOSP.")
	fmt.Fprintln(out, "The manifest is not updated during this filtered sync.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --include-uwuaosp  Include projects using the uwuAOSP remote")
	fmt.Fprintln(out, "  -h, --help         Show this help")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Selectors use project paths, directory prefixes, or shell-style patterns.")
}

func loadProjects(top, repoPath string) ([]project, error) {
	command := exec.Command(repoPath, "manifest", "--format=json")
	command.Dir = top
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return nil, fmt.Errorf("read repo manifest: %s", message)
		}
		return nil, fmt.Errorf("read repo manifest: %w", err)
	}

	var parsed manifest
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("parse repo manifest: %w", err)
	}
	defaultRemote := parsed.Default.Remote
	if defaultRemote == "" {
		defaultRemote = "origin"
	}

	projects := make([]project, 0, len(parsed.Projects))
	seen := make(map[string]bool, len(parsed.Projects))
	for _, item := range parsed.Projects {
		projectPath := item.Path
		if projectPath == "" {
			projectPath = item.Name
		}
		if projectPath == "" || seen[projectPath] {
			continue
		}
		remote := item.Remote
		if remote == "" {
			remote = defaultRemote
		}
		projects = append(projects, project{Path: filepath.ToSlash(projectPath), Remote: remote})
		seen[projectPath] = true
	}
	return projects, nil
}

func findSourceRoot() (string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return findSourceRootFrom(workingDirectory)
}

func findSourceRootFrom(start string) (string, error) {
	directory, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(directory, ".repo")); err == nil && info.IsDir() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", errors.New("Android source root with .repo was not found")
}

func selectProjects(top string, projects []project, args []string, includeUwuaosp bool) ([]string, []string, error) {
	allowed := make([]project, 0, len(projects))
	allPaths := make([]string, 0, len(projects))
	for _, item := range projects {
		allPaths = append(allPaths, item.Path)
		if includeUwuaosp || item.Remote != excludedRemote {
			allowed = append(allowed, item)
		}
	}

	selectors := make([]string, 0)
	syncArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			syncArgs = append(syncArgs, arg)
			continue
		}
		if isPathSelector(arg) {
			selectors = append(selectors, arg)
			continue
		}
		if matchesAnyProject(top, arg, allPaths) {
			selectors = append(selectors, arg)
			continue
		}
		syncArgs = append(syncArgs, arg)
	}

	selected := make([]string, 0, len(allowed))
	seen := make(map[string]bool, len(allowed))
	for _, item := range allowed {
		if len(selectors) != 0 && !matchesSelectors(top, item.Path, selectors) {
			continue
		}
		if !seen[item.Path] {
			selected = append(selected, item.Path)
			seen[item.Path] = true
		}
	}
	for _, selector := range selectors {
		matched := false
		for _, item := range allowed {
			if matchesSelector(top, item.Path, selector) {
				matched = true
				break
			}
		}
		if !matched {
			return nil, nil, fmt.Errorf("no repo projects matched: %s", selector)
		}
	}
	return selected, syncArgs, nil
}

func matchesAnyProject(top, selector string, paths []string) bool {
	for _, projectPath := range paths {
		if matchesSelector(top, projectPath, selector) {
			return true
		}
	}
	return false
}

func matchesSelectors(top, projectPath string, selectors []string) bool {
	for _, selector := range selectors {
		if matchesSelector(top, projectPath, selector) {
			return true
		}
	}
	return false
}

func matchesSelector(top, projectPath, selector string) bool {
	selector = strings.TrimSuffix(filepath.ToSlash(selector), "/")
	if selector == "" {
		return false
	}
	if projectPath == selector || strings.HasPrefix(projectPath, selector+"/") {
		return true
	}
	if shellPatternMatch(selector, projectPath) {
		return true
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(top, filepath.FromSlash(selector)))
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(top, resolved)
	if err != nil {
		return false
	}
	relative = filepath.ToSlash(relative)
	return projectPath == relative || strings.HasPrefix(projectPath, relative+"/")
}

func isPathSelector(arg string) bool {
	return strings.Contains(arg, "/") || strings.ContainsAny(arg, "*?[")
}

func buildSyncArgs(syncArgs, selected []string) []string {
	commandArgs := []string{"sync"}
	if !containsNoManifestUpdate(syncArgs) {
		commandArgs = append(commandArgs, "--no-manifest-update")
	}
	commandArgs = append(commandArgs, syncArgs...)
	return append(commandArgs, selected...)
}

func containsNoManifestUpdate(args []string) bool {
	for _, arg := range args {
		if arg == "--no-manifest-update" || arg == "--nmu" {
			return true
		}
	}
	return false
}

func shellPatternMatch(pattern, value string) bool {
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			expression.WriteString(".*")
		case '?':
			expression.WriteByte('.')
		case '[':
			end := strings.IndexByte(pattern[index+1:], ']')
			if end < 0 {
				expression.WriteString(`\[`)
				continue
			}
			end += index + 1
			class := pattern[index+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			index = end
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteByte('$')
	matched, err := regexp.MatchString(expression.String(), value)
	return err == nil && matched
}
