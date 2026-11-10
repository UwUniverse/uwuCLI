// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	compactTUIRefreshInterval = 33 * time.Millisecond
	compactTUISpinnerInterval = 200 * time.Millisecond
	compactTUILogLines        = 160
	compactTUIWheelStep       = 3
	compactTUILineBytes       = 4096
	compactTUIWriteBuffer     = 256 * 1024
	compactTUICaptureTimeout  = 2 * time.Second
	compactTUIStartupLogLines = 20
)

var compactTUISpinner = []rune{'|', '/', '-', '\\'}

type compactTaskStatus uint8

const (
	compactTaskPending compactTaskStatus = iota
	compactTaskRunning
	compactTaskDone
	compactTaskFailed
)

type compactTask struct {
	name     string
	label    string
	status   compactTaskStatus
	started  time.Time
	duration time.Duration
	jobs     int
	percent  int
	done     int
	total    int
	eta      string
	etaAt    time.Time
	finished int
	activity string
	logs     compactRing
}

type compactRing struct {
	lines []string
	next  int
	full  bool
}

type compactRunningAction struct {
	id          string
	description string
	started     time.Time
}

func (ring *compactRing) len() int {
	if ring.full {
		return len(ring.lines)
	}
	return ring.next
}

func newCompactRing(capacity int) compactRing {
	if capacity < 1 {
		capacity = 1
	}
	return compactRing{lines: make([]string, capacity)}
}

func (ring *compactRing) add(line string) {
	if len(ring.lines) == 0 {
		return
	}
	if len(line) > compactTUILineBytes {
		line = line[len(line)-compactTUILineBytes:]
		line = strings.ToValidUTF8(line, "")
	}
	ring.lines[ring.next] = line
	ring.next = (ring.next + 1) % len(ring.lines)
	if ring.next == 0 {
		ring.full = true
	}
}

func (ring *compactRing) recent(limit int) []string {
	count := ring.next
	start := 0
	if ring.full {
		count = len(ring.lines)
		start = ring.next
	}
	if limit > 0 && count > limit {
		start = (start + count - limit) % len(ring.lines)
		count = limit
	}
	result := make([]string, 0, count)
	for index := 0; index < count; index++ {
		result = append(result, ring.lines[(start+index)%len(ring.lines)])
	}
	return result
}

type compactTUIContextKey struct{}

type compactTUI struct {
	mu                  sync.Mutex
	renderMu            sync.Mutex
	terminal            *os.File
	input               *os.File
	tasks               []*compactTask
	byName              map[string]*compactTask
	active              *compactTask
	selected            int
	details             bool
	systemPanel         bool
	r8                  int
	clangC              int
	clangCXX            int
	kotlinc             int
	javac               int
	rustc               int
	memory              int64
	memoryTotal         int64
	swapFree            int64
	swapTotal           int64
	cpuPercent          float64
	cpuSampled          bool
	load                loadSnapshot
	diskAvailable       int64
	diskAvailableSet    bool
	spinner             int
	spinnerAt           time.Time
	copyMode            bool
	dirty               bool
	rendered            int
	scrollPaused        bool
	scrollOffset        int
	summaries           []string
	messages            compactMessages
	color               bool
	statusSocket        string
	actions             []compactRunningAction
	progressDescription string
	startupLogs         []string
	actionTick          time.Time
	terminalSize        terminalSize
	altScreen           bool

	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
	interrupt func()
}

type compactMessages struct {
	header      string
	taskLabels  map[string]string
	building    string
	pending     string
	failed      string
	jobs        string
	remaining   string
	running     string
	idle        string
	available   string
	memory      string
	system      string
	cpu         string
	load        string
	swap        string
	disk        string
	jobsLabel   string
	buildStatus string
	cpp         string
	c           string
	kotlin      string
	java        string
	rust        string
	r8Label     string
	footer      string
	copyFooter  string
	outputLog   string
	fallback    string
	logWarning  string
}

func compactMessagesForLocale(chinese bool) compactMessages {
	if chinese {
		return compactMessages{
			header:      "[任务]",
			taskLabels:  map[string]string{"Graph": "构建图", "Main": "主构建"},
			building:    "编译中",
			pending:     "等待",
			failed:      "失败",
			jobs:        "并发",
			remaining:   "剩余",
			running:     "运行中",
			idle:        "空闲",
			available:   "可用",
			memory:      "内存",
			system:      "系统状态",
			cpu:         "CPU",
			load:        "负载",
			swap:        "交换空间",
			disk:        "输出盘可用",
			jobsLabel:   "并发",
			buildStatus: "构建情况",
			cpp:         "C++",
			c:           "C",
			kotlin:      "Kotlin",
			java:        "Java",
			rust:        "Rust",
			r8Label:     "R8",
			footer:      "↑↓ 选择   Ctrl+A 日志   Ctrl+S 系统   Ctrl+P 复制   Ctrl+C 停止",
			copyFooter:  "复制模式   Ctrl+P 返回   Ctrl+C 停止",
			outputLog:   "uni: 完整输出日志: %s\n",
			fallback:    "uni: compact TUI 已禁用，使用普通输出: %v\n",
			logWarning:  "uni: 输出日志警告: %v\n",
		}
	}
	return compactMessages{
		header:      "[Task]",
		taskLabels:  map[string]string{"Graph": "Graph", "Main": "Main"},
		building:    "building",
		pending:     "pending",
		failed:      "failed",
		jobs:        "jobs",
		remaining:   "eta",
		running:     "running",
		idle:        "idle",
		available:   "available",
		memory:      "RAM",
		system:      "SYSTEM",
		cpu:         "CPU",
		load:        "LOAD",
		swap:        "SWAP",
		disk:        "DISK FREE",
		jobsLabel:   "jobs",
		buildStatus: "BUILD STATUS",
		cpp:         "C++",
		c:           "C",
		kotlin:      "Kotlin",
		java:        "Java",
		rust:        "Rust",
		r8Label:     "R8",
		footer:      "↑↓ Select   Ctrl+A Logs   Ctrl+S System   Ctrl+P Copy   Ctrl+C Stop",
		copyFooter:  "Copy mode   Ctrl+P Resume   Ctrl+C Stop",
		outputLog:   "uni: output log: %s\n",
		fallback:    "uni: compact TUI disabled; using line output: %v\n",
		logWarning:  "uni: output log warning: %v\n",
	}
}

func compactChineseLocale() bool {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANGUAGE", "LANG"} {
		if locale := strings.ToLower(strings.TrimSpace(os.Getenv(name))); locale != "" {
			return strings.HasPrefix(locale, "zh")
		}
	}
	return false
}

func compactColorEnabled(environment []string) bool {
	if _, set := environmentValue(environment, "NO_COLOR"); set {
		return false
	}
	term, _ := environmentValue(environment, "TERM")
	return term != "" && !strings.EqualFold(term, "dumb")
}

func newCompactTUI(input, terminal *os.File) *compactTUI {
	names := []string{"Graph", "Main"}
	messages := compactMessagesForLocale(compactChineseLocale())
	tui := &compactTUI{
		terminal:     terminal,
		input:        input,
		byName:       make(map[string]*compactTask, len(names)),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		dirty:        true,
		systemPanel:  false,
		spinnerAt:    time.Now(),
		messages:     messages,
		color:        compactColorEnabled(os.Environ()),
		terminalSize: compactTerminalSize(terminal),
	}
	for _, name := range names {
		task := &compactTask{name: name, label: messages.taskLabels[name], status: compactTaskPending, logs: newCompactRing(compactTUILogLines)}
		tui.tasks = append(tui.tasks, task)
		tui.byName[name] = task
	}
	if snapshot, err := ReadMemorySnapshot(); err == nil {
		tui.memory = snapshot.Available
		tui.memoryTotal = snapshot.Total
		tui.swapFree = snapshot.SwapFree
		tui.swapTotal = snapshot.SwapTotal
	}
	if load, err := readLoadSnapshot(); err == nil {
		tui.load = load
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		if available, err := readDiskAvailable(workingDirectory); err == nil {
			tui.diskAvailable = available
			tui.diskAvailableSet = true
		}
	}
	return tui
}

func withCompactTUI(ctx context.Context, tui *compactTUI) context.Context {
	return context.WithValue(ctx, compactTUIContextKey{}, tui)
}

func compactTUIFromContext(ctx context.Context) *compactTUI {
	if ctx == nil {
		return nil
	}
	tui, _ := ctx.Value(compactTUIContextKey{}).(*compactTUI)
	return tui
}

func compactTaskName(phase string) string {
	switch {
	case strings.HasPrefix(phase, "graph-analysis"):
		return "Graph"
	case phase == "ninja", phase == "final", strings.HasPrefix(phase, "segment-"):
		return "Main"
	default:
		return "Main"
	}
}

func (tui *compactTUI) phaseStarted(phase string, jobs int) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	task := tui.byName[compactTaskName(phase)]
	if task.status != compactTaskRunning {
		task.started = time.Now()
		task.duration = 0
		task.percent = 0
		task.done = 0
		task.total = 0
		task.eta = ""
		task.etaAt = time.Time{}
		task.finished = 0
		task.activity = ""
	}
	task.status = compactTaskRunning
	task.jobs = jobs
	tui.active = task
	tui.dirty = true
}

func (tui *compactTUI) phaseFinished(phase string, err error) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	task := tui.byName[compactTaskName(phase)]
	if err != nil {
		task.status = compactTaskFailed
	} else if !strings.HasPrefix(phase, "segment-") {
		task.status = compactTaskDone
	}
	if !task.started.IsZero() {
		task.duration = time.Since(task.started)
	}
	tui.dirty = true
}

func (tui *compactTUI) updateTelemetry(sample TelemetrySample) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if sample.MemoryAvailable > 0 {
		tui.memory = sample.MemoryAvailable
	}
	if sample.MemoryTotal > 0 {
		tui.memoryTotal = sample.MemoryTotal
	}
	if sample.SwapFree > 0 {
		tui.swapFree = sample.SwapFree
	}
	if sample.SwapTotal > 0 {
		tui.swapTotal = sample.SwapTotal
	}
	if !sample.Timestamp.IsZero() {
		tui.cpuPercent = sample.CPUPercent
		tui.cpuSampled = true
		if sample.Load.one != "" {
			tui.load = sample.Load
		}
		if sample.DiskAvailable > 0 {
			tui.diskAvailable = sample.DiskAvailable
			tui.diskAvailableSet = true
		}
	}
	tui.r8 = sample.R8
	tui.clangC = sample.ClangC
	tui.clangCXX = sample.ClangCXX
	tui.kotlinc = sample.Kotlinc
	tui.javac = sample.Javac
	tui.rustc = sample.Rustc
	if tui.active != nil && tui.active.status == compactTaskRunning {
		tui.active.activity = compactActivity(sample)
	}
	tui.dirty = true
}

func (tui *compactTUI) updateParallelism(jobs int) {
	if jobs < 1 {
		return
	}
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if tui.active != nil && tui.active.status == compactTaskRunning {
		tui.active.jobs = jobs
		tui.dirty = true
	}
}

func compactActivity(sample TelemetrySample) string {
	switch {
	case sample.Linker > 0:
		return fmt.Sprintf("link=%d", sample.Linker)
	case sample.ClangCXX > 0:
		return fmt.Sprintf("C++=%d", sample.ClangCXX)
	case sample.ClangC > 0:
		return fmt.Sprintf("C=%d", sample.ClangC)
	case sample.Kotlinc > 0:
		return fmt.Sprintf("Kotlin=%d", sample.Kotlinc)
	case sample.Javac > 0:
		return fmt.Sprintf("Java=%d", sample.Javac)
	case sample.Rustc > 0:
		return fmt.Sprintf("Rust=%d", sample.Rustc)
	case sample.Clang > 0:
		return fmt.Sprintf("C/C++=%d", sample.Clang)
	default:
		return ""
	}
}

func (tui *compactTUI) consume(line string) {
	var displayLine string
	if compactLineUnchanged(line) {
		line = strings.TrimSpace(line)
		displayLine = line
	} else {
		displayLine = rewriteCompactDisplayLine(line)
		line = rewriteCompactPlainLine(line)
	}
	if line == "" {
		return
	}
	tui.mu.Lock()
	defer tui.mu.Unlock()
	target := tui.active

	switch {
	case strings.HasPrefix(line, "uni: reuse graph"):
		task := tui.byName["Graph"]
		task.status = compactTaskDone
		task.duration = 0
		target = task
		if tui.active == nil || tui.active == task || tui.active.status != compactTaskRunning {
			tui.active = task
		}
	case strings.Contains(line, "Analyzing Android.bp files"):
		task := tui.byName["Graph"]
		if task.status == compactTaskPending {
			task.status = compactTaskRunning
			task.started = time.Now()
		}
		target = task
		if tui.active == nil || tui.active == task || tui.active.status != compactTaskRunning {
			tui.active = task
		}
	}

	if tui.active == nil {
		tui.active = tui.byName["Graph"]
	}
	if target == nil {
		target = tui.active
	}
	if displayLine != "" {
		target.activity = ""
	}
	if strings.HasPrefix(line, "uni: startup:") {
		tui.startupLogs = append(tui.startupLogs, line)
		if len(tui.startupLogs) > compactTUIStartupLogLines {
			tui.startupLogs = tui.startupLogs[len(tui.startupLogs)-compactTUIStartupLogLines:]
		}
	}
	target.logs.add(line)
	if percent, done, total, ok, eta := parseCompactStatus(line); ok {
		target.percent = percent
		target.done = done
		target.total = total
		target.eta = eta
		if target.eta != "" {
			if duration, err := time.ParseDuration(target.eta); err == nil {
				target.etaAt = time.Now().Add(duration)
			}
		} else {
			target.etaAt = time.Time{}
		}
	}
	if strings.HasPrefix(line, "uni: phases=") || strings.HasPrefix(line, "uni: package=") ||
		strings.HasPrefix(line, "uni: output=") || strings.HasPrefix(line, "#### build completed successfully") {
		summary := line
		if strings.HasPrefix(line, "#### build completed successfully") && displayLine != "" {
			summary = displayLine
		}
		tui.summaries = append(tui.summaries, summary)
	}
	if strings.HasPrefix(line, "FAILED:") || strings.HasPrefix(line, "FAILED ") || strings.Contains(line, "ninja failed") {
		tui.summaries = append(tui.summaries, line)
	}
	tui.dirty = true
}

func (tui *compactTUI) consumeStatusEvent(event compactStatusEvent, connectionID uint64) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	task := tui.active
	if task == nil {
		task = tui.byName["Main"]
	}
	switch event.Type {
	case "reset":
		task.percent = 0
		task.done = 0
		task.total = 0
		task.eta = ""
		task.etaAt = time.Time{}
		task.finished = 0
		tui.progressDescription = ""
	case "total":
		if event.Total > 0 {
			task.total = event.Total
			task.done = task.finished
			task.percent = task.done * 100 / event.Total
		}
	case "estimate":
		if event.EstimatedTimeUnixNano > 0 {
			task.etaAt = time.Unix(0, event.EstimatedTimeUnixNano)
			task.eta = ""
		}
	case "start":
		tui.startupLogs = nil
		started := time.Now()
		if event.StartedUnixNano > 0 {
			started = time.Unix(0, event.StartedUnixNano)
		}
		tui.actions = append(tui.actions, compactRunningAction{
			id:          fmt.Sprintf("%d:%d", connectionID, event.ID),
			description: sanitizeCompactLine(event.Description),
			started:     started,
		})
		tui.progressDescription = sanitizeCompactLine(event.Description)
	case "finish":
		id := fmt.Sprintf("%d:%d", connectionID, event.ID)
		for index := range tui.actions {
			if tui.actions[index].id == id {
				tui.actions = append(tui.actions[:index], tui.actions[index+1:]...)
				break
			}
		}
		task.finished++
		task.done = task.finished
		if task.total > 0 {
			task.percent = task.done * 100 / task.total
		}
		if event.Description != "" {
			tui.progressDescription = sanitizeCompactLine(event.Description)
		}
	case "disconnect":
		prefix := fmt.Sprintf("%d:", connectionID)
		for index := 0; index < len(tui.actions); {
			if strings.HasPrefix(tui.actions[index].id, prefix) {
				tui.actions = append(tui.actions[:index], tui.actions[index+1:]...)
				continue
			}
			index++
		}
	}
	tui.dirty = true
}

func compactDisplayLine(line string) string {
	if compactLineUnchanged(line) {
		return strings.TrimSpace(line)
	}
	return rewriteCompactDisplayLine(line)
}

func rewriteCompactDisplayLine(line string) string {
	line = strings.ToValidUTF8(line, "")
	var output strings.Builder
	output.Grow(len(line))
	for index := 0; index < len(line); {
		if line[index] == 0x1b {
			if index+1 < len(line) && line[index+1] == '[' {
				end := index + 2
				for end < len(line) && !(line[end] >= 0x40 && line[end] <= 0x7e) {
					end++
				}
				if end < len(line) {
					if line[end] == 'm' {
						output.WriteString(line[index : end+1])
					}
					index = end + 1
					continue
				}
			}
			index++
			continue
		}
		value := line[index]
		index++
		if value == '\t' {
			output.WriteString("    ")
		} else if value >= 0x20 || value >= utf8.RuneSelf {
			output.WriteByte(value)
		}
	}
	return strings.TrimSpace(output.String())
}

// Ordinary output already has the representation needed by both the display and log ring.
func compactLineUnchanged(line string) bool {
	nonASCII := false
	for index := 0; index < len(line); index++ {
		if line[index] < 0x20 {
			return false
		}
		nonASCII = nonASCII || line[index] >= utf8.RuneSelf
	}
	return !nonASCII || utf8.ValidString(line)
}

func parseCompactProgress(line string) (percent, done, total int, ok bool) {
	percent, done, total, ok, _ = parseCompactStatus(line)
	return
}

func parseCompactETA(line string) string {
	_, _, _, _, eta := parseCompactStatus(line)
	return eta
}

// Progress and ETA have independent first-match rules, but can share one scan.
func parseCompactStatus(line string) (percent, done, total int, ok bool, eta string) {
	for offset := 0; offset < len(line); {
		open := strings.IndexByte(line[offset:], '[')
		if open < 0 {
			break
		}
		open += offset
		close := strings.IndexByte(line[open+1:], ']')
		if close < 0 {
			break
		}
		close += open + 1
		var fields [3]string
		count := 0
		for field := range strings.FieldsSeq(line[open+1 : close]) {
			fields[count] = field
			count++
			if count == len(fields) {
				break
			}
		}
		if count >= 2 && strings.HasSuffix(fields[0], "%") {
			if !ok {
				parsedPercent, percentErr := strconv.Atoi(strings.TrimSuffix(fields[0], "%"))
				left, right, found := strings.Cut(fields[1], "/")
				parsedDone, doneErr := strconv.Atoi(left)
				parsedTotal, totalErr := strconv.Atoi(right)
				if found && percentErr == nil && doneErr == nil && totalErr == nil && parsedTotal > 0 {
					percent, done, total, ok = parsedPercent, parsedDone, parsedTotal, true
				}
			}
			if eta == "" && count >= 3 && strings.Contains(fields[1], "/") {
				if _, err := time.ParseDuration(fields[2]); err == nil {
					eta = fields[2]
				}
			}
		}
		if ok && eta != "" {
			break
		}
		offset = close + 1
	}
	return
}

func sanitizeCompactLine(line string) string {
	if compactLineUnchanged(line) {
		return strings.TrimSpace(line)
	}
	return rewriteCompactPlainLine(line)
}

func rewriteCompactPlainLine(line string) string {
	line = strings.ToValidUTF8(line, "")
	var output strings.Builder
	output.Grow(len(line))
	for index := 0; index < len(line); {
		if line[index] == 0x1b {
			index++
			if index >= len(line) {
				break
			}
			switch line[index] {
			case '[':
				index++
				for index < len(line) {
					value := line[index]
					index++
					if value >= 0x40 && value <= 0x7e {
						break
					}
				}
			case ']':
				index++
				for index < len(line) {
					if line[index] == 0x07 {
						index++
						break
					}
					if line[index] == 0x1b && index+1 < len(line) && line[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
			default:
				index++
			}
			continue
		}
		value := line[index]
		index++
		if value == '\t' {
			output.WriteString("    ")
		} else if value >= 0x20 || value >= utf8.RuneSelf {
			output.WriteByte(value)
		}
	}
	return strings.TrimSpace(output.String())
}

func compactDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	seconds := int64(duration / time.Second)
	if seconds >= 3600 {
		return fmt.Sprintf("%dh%02dm%02ds", seconds/3600, seconds%3600/60, seconds%60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

func compactMemory(value int64) string {
	if value <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%.1fG", float64(value)/float64(gibibyte))
}

func truncateCompactLine(line string, width int) string {
	if width < 1 {
		return ""
	}
	if compactTextWidth(line) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	var output strings.Builder
	used := 0
	for _, value := range line {
		valueWidth := compactRuneWidth(value)
		if used+valueWidth > width-1 {
			break
		}
		output.WriteRune(value)
		used += valueWidth
	}
	return output.String() + "…"
}

func compactRuneWidth(value rune) int {
	if value == 0 {
		return 0
	}
	if value >= 0x1100 && (value <= 0x115f || value == 0x2329 || value == 0x232a ||
		(value >= 0x2e80 && value <= 0xa4cf) || (value >= 0xac00 && value <= 0xd7a3) ||
		(value >= 0xf900 && value <= 0xfaff) || (value >= 0xfe10 && value <= 0xfe6f) ||
		(value >= 0xff00 && value <= 0xff60) || (value >= 0xffe0 && value <= 0xffe6) ||
		(value >= 0x1f300 && value <= 0x1faff) || (value >= 0x20000 && value <= 0x3fffd)) {
		return 2
	}
	return 1
}

func compactTextWidth(text string) int {
	width := 0
	for _, value := range text {
		width += compactRuneWidth(value)
	}
	return width
}

func compactPadRight(text string, width int) string {
	if padding := width - compactTextWidth(text); padding > 0 {
		return text + strings.Repeat(" ", padding)
	}
	return text
}

func compactANSI(enabled bool, style, value string) string {
	if !enabled || value == "" {
		return value
	}
	return "\x1b[" + style + "m" + value + "\x1b[0m"
}

type compactRGB struct {
	r int
	g int
	b int
}

var (
	compactThemeBlue     = compactRGB{r: 118, g: 177, b: 255}
	compactThemePink     = compactRGB{r: 255, g: 132, b: 196}
	compactThemeCanvas   = compactRGB{r: 14, g: 19, b: 35}
	compactThemePanel    = compactRGB{r: 27, g: 34, b: 56}
	compactThemeBuild    = compactRGB{r: 34, g: 39, b: 64}
	compactThemeSelected = compactRGB{r: 48, g: 34, b: 58}
	compactThemeProgress = compactRGB{r: 35, g: 43, b: 70}
	compactThemeAction   = compactRGB{r: 20, g: 28, b: 48}
)

func compactThemeANSI(enabled bool, style string, color compactRGB, value string) string {
	if !enabled || value == "" {
		return value
	}
	return fmt.Sprintf("\x1b[%s;38;2;%d;%d;%dm%s\x1b[0m", style, color.r, color.g, color.b, value)
}

func compactBackgroundLine(enabled bool, color compactRGB, line string, width int) string {
	if !enabled {
		return line
	}
	visibleWidth := compactTextWidth(sanitizeCompactLine(line))
	// Leave the last terminal column untouched to avoid delayed-wrap scrolling.
	padding := max(width-1-visibleWidth, 0)
	background := fmt.Sprintf("\x1b[48;2;%d;%d;%dm", color.r, color.g, color.b)
	restoreBackground := fmt.Sprintf("\x1b[22;39;48;2;%d;%d;%dm", color.r, color.g, color.b)
	line = strings.ReplaceAll(line, "\x1b[0m", restoreBackground)
	line = strings.ReplaceAll(line, "\x1b[m", restoreBackground)
	return background + line + strings.Repeat(" ", padding) + "\x1b[0m"
}

var compactBuildTemperature = []struct {
	elapsed time.Duration
	color   compactRGB
}{
	{0, compactRGB{255, 255, 255}},
	{10 * time.Second, compactRGB{255, 246, 232}},
	{30 * time.Second, compactRGB{255, 235, 150}},
	{60 * time.Second, compactRGB{255, 195, 45}},
	{100 * time.Second, compactRGB{255, 112, 20}},
	{150 * time.Second, compactRGB{255, 38, 38}},
}

func compactBuildTemperatureColor(elapsed time.Duration) compactRGB {
	if elapsed <= compactBuildTemperature[0].elapsed {
		return compactBuildTemperature[0].color
	}
	for index := 1; index < len(compactBuildTemperature); index++ {
		next := compactBuildTemperature[index]
		if elapsed > next.elapsed {
			continue
		}
		previous := compactBuildTemperature[index-1]
		progress := float64(elapsed-previous.elapsed) / float64(next.elapsed-previous.elapsed)
		return compactRGB{
			r: previous.color.r + int(float64(next.color.r-previous.color.r)*progress),
			g: previous.color.g + int(float64(next.color.g-previous.color.g)*progress),
			b: previous.color.b + int(float64(next.color.b-previous.color.b)*progress),
		}
	}
	return compactBuildTemperature[len(compactBuildTemperature)-1].color
}

func compactTemperatureANSI(enabled bool, elapsed time.Duration, value string) string {
	if !enabled || value == "" {
		return value
	}
	color := compactBuildTemperatureColor(elapsed)
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", color.r, color.g, color.b, value)
}

func compactBold(value string) string {
	if value == "" {
		return value
	}
	return "\x1b[1m" + value + "\x1b[0m"
}

func (tui *compactTUI) taskLine(task *compactTask) string {
	return tui.taskLineWithActivity(task, true)
}

func (tui *compactTUI) taskProgressFallback(task *compactTask) string {
	return tui.taskLineWithActivity(task, false)
}

func (tui *compactTUI) taskLineWithActivity(task *compactTask, includeActivity bool) string {
	duration := task.duration
	if task.status == compactTaskRunning && !task.started.IsZero() {
		duration = time.Since(task.started)
	}
	prefix := compactPadRight(task.label, 9)
	switch task.status {
	case compactTaskDone:
		line := fmt.Sprintf("%s ■ %s", prefix, compactDuration(duration))
		if task.total > 0 {
			line += fmt.Sprintf("  %d%% %d/%d", task.percent, task.done, task.total)
		}
		return line
	case compactTaskFailed:
		return fmt.Sprintf("%s × %s  %s", prefix, tui.messages.failed, compactDuration(duration))
	case compactTaskRunning:
		spinner := string(compactTUISpinner[tui.spinner%len(compactTUISpinner)])
		durationText := compactDuration(duration)
		line := fmt.Sprintf("%s %s %s  %s  %s=%d", prefix, spinner, tui.messages.building, durationText, tui.messages.jobs, task.jobs)
		if includeActivity && task.activity != "" {
			line += "  " + task.activity
		}
		if task.total > 0 {
			line += fmt.Sprintf("  %d%% %d/%d", task.percent, task.done, task.total)
		}
		if task.eta != "" {
			line += fmt.Sprintf("  %s %s", tui.messages.remaining, task.eta)
		} else if !task.etaAt.IsZero() {
			remaining := time.Until(task.etaAt)
			if remaining < 0 {
				remaining = 0
			}
			line += fmt.Sprintf("  %s %s", tui.messages.remaining, compactDuration(remaining))
		}
		return line
	default:
		return fmt.Sprintf("%s □ %s", prefix, tui.messages.pending)
	}
}

func (tui *compactTUI) systemPanelLines(lineWidth, bodyHeight int) []string {
	load := "—"
	if tui.load.one != "" {
		load = tui.load.one + " / " + tui.load.five + " / " + tui.load.fifteen
	}
	cpu := "—"
	if tui.cpuSampled {
		cpu = fmt.Sprintf("%.0f%%", tui.cpuPercent)
	}
	memoryTotal := compactMemory(tui.memoryTotal)
	memoryAvailable := compactMemory(tui.memory)
	memoryUsed := "—"
	if tui.memoryTotal > 0 {
		memoryUsed = compactMemory(max(tui.memoryTotal-tui.memory, 0))
	}
	swapTotal := compactMemory(tui.swapTotal)
	swapFree := compactMemory(tui.swapFree)
	swapUsed := "—"
	if tui.swapTotal > 0 {
		swapUsed = compactMemory(max(tui.swapTotal-tui.swapFree, 0))
	}
	diskFree := "—"
	if tui.diskAvailableSet {
		diskFree = compactMemory(tui.diskAvailable)
	}
	jobs := 0
	if tui.active != nil {
		jobs = tui.active.jobs
	}
	if !tui.systemPanel || bodyHeight < 15 {
		line := fmt.Sprintf("  %s  CPU %s   %s %s   %s %s   %s %d", tui.messages.system,
			cpu, tui.messages.memory, memoryAvailable, tui.messages.swap, swapFree, tui.messages.jobsLabel, jobs)
		return []string{truncateCompactLine(line, lineWidth)}
	}
	lines := []string{
		fmt.Sprintf("  ◆ %s", tui.messages.system),
		fmt.Sprintf("     CPU  %s      %s  %s", cpu, tui.messages.load, load),
		fmt.Sprintf("     %s  %s / %s      %s  %s", tui.messages.memory, memoryUsed, memoryTotal, tui.messages.available, memoryAvailable),
		fmt.Sprintf("     %s  %s / %s      %s  %s", tui.messages.swap, swapUsed, swapTotal, tui.messages.available, swapFree),
		fmt.Sprintf("     %s  %s      %s  %d", tui.messages.disk, diskFree, tui.messages.jobsLabel, jobs),
	}
	for index := range lines {
		lines[index] = truncateCompactLine(lines[index], lineWidth)
		if index == 0 {
			lines[index] = compactThemeANSI(tui.color, "1", compactThemePink, lines[index])
			continue
		}
		for _, label := range []string{tui.messages.cpu, tui.messages.load, tui.messages.memory,
			tui.messages.swap, tui.messages.disk, tui.messages.jobsLabel} {
			lines[index] = strings.Replace(lines[index], label, compactThemeANSI(tui.color, "1", compactThemeBlue, label), 1)
		}
	}
	return lines
}

func (tui *compactTUI) buildPanelLines(lineWidth, _ int) []string {
	counts := []struct {
		label string
		count int
	}{
		{tui.messages.cpp, tui.clangCXX},
		{tui.messages.c, tui.clangC},
		{tui.messages.kotlin, tui.kotlinc},
		{tui.messages.java, tui.javac},
		{tui.messages.rust, tui.rustc},
		{tui.messages.r8Label, tui.r8},
	}
	lines := []string{truncateCompactLine("  ◆ "+tui.messages.buildStatus, lineWidth)}
	for index := 0; index < len(counts); index += 2 {
		left := counts[index]
		right := counts[index+1]
		lines = append(lines, truncateCompactLine(fmt.Sprintf("     %-8s %d    %-8s %d", left.label, left.count, right.label, right.count), lineWidth))
	}
	for index := range lines {
		if index == 0 {
			lines[index] = compactThemeANSI(tui.color, "1", compactThemePink, lines[index])
			continue
		}
		for _, item := range counts {
			lines[index] = strings.Replace(lines[index], item.label, compactThemeANSI(tui.color, "1", compactThemeBlue, item.label), 1)
		}
	}
	return lines
}

func (tui *compactTUI) frame(force bool) string {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if !force && !tui.dirty {
		return ""
	}
	tui.dirty = false
	size := compactTerminalSize(tui.terminal)
	tui.terminalSize = size
	width := int(size.cols)
	if width < 40 {
		width = 40
	}
	lineWidth := width - 3
	rows := int(size.rows)
	if rows < 1 {
		rows = 24
	}
	paneHeight := compactActionPaneHeight(rows)
	bodyHeight := rows - paneHeight
	if bodyHeight < 0 {
		bodyHeight = 0
	}
	body := make([]string, 0, bodyHeight)
	header := truncateCompactLine(tui.messages.header, lineWidth)
	header = compactThemeANSI(tui.color, "1", compactThemeBlue, header)
	body = append(body, header)
	for index, task := range tui.tasks {
		marker := "  "
		if index == tui.selected {
			marker = "→ "
		}
		taskSummary := tui.taskLine(task)
		if task.status == compactTaskRunning && task == tui.active {
			taskSummary = task.label
		}
		line := truncateCompactLine(marker+taskSummary, lineWidth)
		if tui.color {
			style := "2"
			switch task.status {
			case compactTaskRunning:
				style = "1"
			case compactTaskDone:
				style = "32"
			case compactTaskFailed:
				style = "1;31"
			}
			if style != "" {
				if task.status == compactTaskRunning {
					line = compactThemeANSI(true, style, compactThemeBlue, line)
				} else {
					line = "\x1b[" + style + "m" + line + "\x1b[0m"
				}
			}
			if index == tui.selected {
				selection := "→ " + task.label
				marker := compactThemeANSI(true, "1", compactThemePink, selection)
				line = strings.Replace(line, selection, marker+"\x1b["+style+"m", 1)
			}
		}
		body = append(body, line)
	}
	buildStart := len(body)
	for _, line := range tui.buildPanelLines(lineWidth, bodyHeight) {
		body = append(body, line)
	}
	buildEnd := len(body)
	systemStart := len(body)
	systemLines := tui.systemPanelLines(lineWidth, bodyHeight)
	for _, line := range systemLines {
		body = append(body, line)
	}
	systemEnd := len(body)
	if len(body) > bodyHeight {
		body = body[:bodyHeight]
	}
	if bodyHeight > len(body) {
		body = append(body, make([]string, bodyHeight-len(body))...)
	}
	if tui.details {
		logStart := systemEnd
		logEnd := bodyHeight
		if logStart < logEnd && tui.selected >= 0 && tui.selected < len(tui.tasks) {
			ring := &tui.tasks[tui.selected].logs
			lineCount := ring.len()
			if tui.scrollOffset > lineCount {
				tui.scrollOffset = lineCount
			}
			available := logEnd - logStart
			lines := ring.recent(available + tui.scrollOffset)
			end := len(lines) - tui.scrollOffset
			if end < 0 {
				end = 0
			}
			start := end - available
			if start < 0 {
				start = 0
			}
			for index, line := range lines[start:end] {
				body[logStart+index] = truncateCompactLine(line, lineWidth)
			}
		}
	}

	page := append([]string(nil), body...)
	if paneHeight > 0 {
		page = append(page, compactActionPane(tui, paneHeight, width, lineWidth)...)
	}
	if tui.color {
		for index, line := range page {
			background := compactThemeCanvas
			switch {
			case index >= buildStart && index < buildEnd:
				background = compactThemeBuild
			case index >= systemStart && index < systemEnd:
				background = compactThemePanel
			case index == tui.selected+1:
				background = compactThemeSelected
			case index == len(body):
				background = compactThemeProgress
			case index > len(body):
				background = compactThemeAction
			}
			page[index] = compactBackgroundLine(true, background, line, width)
		}
	}
	return strings.Join(page, "\n")
}

func compactActionPaneHeight(rows int) int {
	if rows < 3 {
		return 0
	}
	return rows / 3
}

func compactSoongProgressLine(task *compactTask, description string, fallback string) string {
	if task == nil || task.total <= 0 {
		if description != "" {
			return description
		}
		return fallback
	}
	percent := task.done * 100 / task.total
	line := fmt.Sprintf("[%3d%% %d/%d", percent, task.done, task.total)
	eta := task.eta
	if eta == "" && !task.etaAt.IsZero() {
		remaining := time.Until(task.etaAt).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		eta = remaining.String()
	}
	if eta != "" {
		line += " " + eta + " remaining"
	}
	line += "]"
	if description != "" {
		line += " " + description
	}
	return line
}

func compactActionPane(tui *compactTUI, height, width, lineWidth int) []string {
	if height < 1 {
		return nil
	}
	rows := make([]string, height)
	var current *compactTask
	if tui.active != nil {
		current = tui.active
	} else {
		for _, task := range tui.tasks {
			if task.status == compactTaskRunning {
				current = task
				break
			}
		}
	}
	if current == nil && len(tui.tasks) > 0 {
		current = tui.tasks[len(tui.tasks)-1]
	}
	if height > 1 && current != nil {
		status := compactSoongProgressLine(current, tui.progressDescription, tui.taskProgressFallback(current))
		line := truncateCompactLine(strings.TrimLeft(status, " "), lineWidth)
		style := "1"
		if current.status == compactTaskRunning {
			style = "1"
		} else if current.status == compactTaskFailed {
			style = "1;31"
		} else if current.status == compactTaskDone {
			style = "1;32"
		}
		if tui.color {
			if current.status == compactTaskRunning {
				rows[0] = compactThemeANSI(true, "1", compactThemeBlue, line)
			} else {
				rows[0] = compactANSI(true, style, line)
			}
		} else {
			rows[0] = compactBold(line)
		}
	}
	visibleRows := max(height-2, 0)
	if len(tui.startupLogs) > 0 {
		count := min(visibleRows, len(tui.startupLogs))
		logs := tui.startupLogs[len(tui.startupLogs)-count:]
		for index, logLine := range logs {
			rows[index+1] = truncateCompactLine("   "+logLine, lineWidth)
		}
		visibleRows -= count
		for index := 0; index < visibleRows && index < len(tui.actions); index++ {
			action := tui.actions[index]
			rows[index+1+count] = compactActionLine(tui, action, lineWidth)
		}
	} else {
		for index := 0; index < visibleRows && index < len(tui.actions); index++ {
			rows[index+1] = compactActionLine(tui, tui.actions[index], lineWidth)
		}
	}
	footer := tui.messages.footer
	if tui.copyMode {
		footer = tui.messages.copyFooter
	}
	rows[height-1] = compactANSI(tui.color, "2", truncateCompactLine(footer, lineWidth))
	return rows
}

func compactActionLine(tui *compactTUI, action compactRunningAction, lineWidth int) string {
	elapsed := time.Since(action.started).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int(elapsed.Seconds())
	duration := fmt.Sprintf("%2d:%02d ", seconds/60, seconds%60)
	description := action.description
	if description == "" {
		description = "action"
	}
	line := truncateCompactLine("   "+duration+description, lineWidth)
	if tui.color {
		line = strings.Replace(line, duration, compactTemperatureANSI(true, time.Since(action.started), duration), 1)
	}
	return line
}

func compactFrameLines(frame string) int {
	if frame == "" {
		return 0
	}
	return strings.Count(frame, "\n") + 1
}

func truncateCompactDisplayLine(line string, width int) string {
	if width < 1 {
		return ""
	}
	visible := sanitizeCompactLine(line)
	if compactTextWidth(visible) <= width {
		return line
	}
	return truncateCompactLine(visible, width)
}

func (tui *compactTUI) render(force bool) {
	tui.renderMu.Lock()
	defer tui.renderMu.Unlock()
	frame := tui.frame(force)
	if frame == "" || tui.terminal == nil {
		return
	}
	lines := strings.Split(frame, "\n")
	var output strings.Builder
	output.Grow(len(frame) + len(lines)*16)
	rows := int(tui.terminalSize.rows)
	if rows < len(lines) {
		rows = len(lines)
	}
	bodyRows := rows - compactActionPaneHeight(rows)
	if bodyRows < 1 {
		bodyRows = rows
	}
	logTop := bodyRows + 1
	if logTop > rows {
		logTop = rows
	}
	// Keep scrolling confined to the bottom log pane; leaked build output must not move the dashboard.
	output.WriteString(fmt.Sprintf("\x1b[%d;%dr", logTop, rows))
	for index, line := range lines {
		output.WriteString(fmt.Sprintf("\x1b[%d;1H\x1b[2K", index+1))
		output.WriteString(line)
	}
	// Keep stray writes below the body scroll region so they cannot shift panel rows.
	output.WriteString(fmt.Sprintf("\x1b[%d;1H", rows))
	_, _ = io.WriteString(tui.terminal, output.String())
	tui.rendered = len(lines)
}

func (tui *compactTUI) animate() {
	tui.mu.Lock()
	if tui.copyMode {
		tui.mu.Unlock()
		return
	}
	shouldRender := tui.dirty
	now := time.Now()
	size := compactTerminalSize(tui.terminal)
	if size != tui.terminalSize {
		tui.terminalSize = size
		tui.dirty = true
		shouldRender = true
	}
	if len(tui.actions) > 0 && now.Sub(tui.actionTick) >= time.Second {
		tui.actionTick = now
		tui.dirty = true
		shouldRender = true
	}
	if !tui.scrollPaused {
		for _, task := range tui.tasks {
			if task.status == compactTaskRunning {
				if time.Since(tui.spinnerAt) >= compactTUISpinnerInterval {
					tui.spinner = (tui.spinner + 1) % len(compactTUISpinner)
					tui.spinnerAt = time.Now()
					tui.dirty = true
					shouldRender = true
				}
				break
			}
		}
	}
	tui.mu.Unlock()
	if shouldRender {
		tui.render(false)
	}
}

func (tui *compactTUI) start() {
	_, _ = io.WriteString(tui.terminal, "\x1b[?1049h\x1b[>1u\x1b[?25l\x1b[?1000h\x1b[?1006h")
	tui.altScreen = true
	tui.render(true)
	go tui.inputLoop()
	go func() {
		defer close(tui.done)
		ticker := time.NewTicker(compactTUIRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				tui.animate()
			case <-tui.stop:
				return
			}
		}
	}()
}

func (tui *compactTUI) inputLoop() {
	buffer := make([]byte, 16)
	var pending []byte
	for {
		select {
		case <-tui.stop:
			return
		default:
		}
		count, err := tui.input.Read(buffer)
		if count > 0 {
			pending = append(pending, buffer[:count]...)
			pending = tui.handleInput(pending)
		}
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			// VMIN=0 and VTIME>0 makes an idle terminal read return zero bytes,
			// which os.File reports as io.EOF. Keep polling for keyboard input.
			time.Sleep(5 * time.Millisecond)
		} else if count == 0 {
			time.Sleep(1 * time.Millisecond)
		}
	}
}

func (tui *compactTUI) handleInput(input []byte) []byte {
	for len(input) > 0 {
		switch {
		case input[0] == 0x01:
			tui.toggleDetails()
			input = input[1:]
		case input[0] == 0x10:
			tui.toggleCopyMode()
			input = input[1:]
		case input[0] == 0x13:
			tui.toggleSystemPanel()
			input = input[1:]
		case input[0] == 'k' || input[0] == 'K':
			tui.resumeLiveView()
			tui.moveSelection(-1)
			input = input[1:]
		case input[0] == 'j' || input[0] == 'J':
			tui.resumeLiveView()
			tui.moveSelection(1)
			input = input[1:]
		case input[0] == 0x1b:
			if len(input) == 1 {
				return input
			}
			if input[1] == '[' && len(input) > 2 && input[2] == '<' {
				final := -1
				for index := 3; index < len(input); index++ {
					if input[index] == 'M' || input[index] == 'm' {
						final = index
						break
					}
				}
				if final < 0 {
					return input
				}
				fields := strings.Split(string(input[3:final]), ";")
				if len(fields) > 0 {
					button, err := strconv.Atoi(fields[0])
					if err == nil {
						tui.handleMouseWheel(button)
					}
				}
				input = input[final+1:]
				continue
			}
			if input[1] == '[' && len(input) > 2 && input[2] >= '0' && input[2] <= '9' {
				final := -1
				for index := 2; index < len(input); index++ {
					if input[index] >= 0x40 && input[index] <= 0x7e {
						final = index
						break
					}
				}
				if final < 0 {
					return input
				}
				sequence := string(input[2 : final+1])
				handled := false
				if strings.HasSuffix(sequence, "u") {
					handled = true
					// Kitty keyboard protocol encodes Ctrl+A as CSI 97;5u.
					fields := strings.Split(strings.TrimSuffix(sequence, "u"), ";")
					if len(fields) >= 2 {
						code, codeErr := strconv.Atoi(fields[0])
						modifiers, modifierErr := strconv.Atoi(fields[1])
						if codeErr == nil && modifierErr == nil && modifiers&4 != 0 {
							switch code {
							case int('a'):
								tui.toggleDetails()
							case int('p'):
								tui.toggleCopyMode()
							case int('s'):
								tui.toggleSystemPanel()
							case int('c'):
								if tui.interrupt != nil {
									tui.interrupt()
								}
							}
						}
					}
				} else if strings.HasSuffix(sequence, "M") || strings.HasSuffix(sequence, "m") {
					handled = true
					// xterm's legacy mouse protocol omits the SGR '<' marker.
					fields := strings.Split(sequence[:len(sequence)-1], ";")
					if len(fields) >= 1 {
						button, buttonErr := strconv.Atoi(fields[0])
						if buttonErr == nil {
							tui.handleMouseWheel(button)
						}
					}
				}
				if handled {
					input = input[final+1:]
					continue
				}
			}
			if input[1] != '[' && input[1] != 'O' {
				input = input[1:]
				continue
			}
			final := -1
			for index := 2; index < len(input); index++ {
				if input[index] >= 0x40 && input[index] <= 0x7e {
					final = index
					break
				}
			}
			if final < 0 {
				return input
			}
			params := string(input[2:final])
			if strings.HasSuffix(params, "~") {
				params = strings.TrimSuffix(params, "~")
			}
			switch input[final] {
			case 'A':
				tui.resumeLiveView()
				tui.moveSelection(-1)
			case 'B':
				tui.resumeLiveView()
				tui.moveSelection(1)
			case '~':
				fields := strings.Split(params, ";")
				if len(fields) >= 3 && fields[0] == "27" {
					modifiers, modifierErr := strconv.Atoi(fields[1])
					code, codeErr := strconv.Atoi(fields[2])
					if modifierErr == nil && codeErr == nil && tui.handleModifiedKey(code, modifiers) {
						input = input[final+1:]
						continue
					}
				}
				code, err := strconv.Atoi(fields[0])
				if err == nil {
					switch code {
					case 1, 7:
						tui.setHistoryOffsetMax()
					case 4, 8:
						tui.resumeLiveView()
					case 5:
						tui.scrollHistory(1)
					case 6:
						tui.scrollHistory(-1)
					}
				}
			}
			input = input[final+1:]
		default:
			input = input[1:]
		}
	}
	return input
}

func (tui *compactTUI) toggleDetails() {
	tui.mu.Lock()
	tui.details = !tui.details
	tui.scrollOffset = 0
	tui.scrollPaused = false
	tui.dirty = true
	tui.mu.Unlock()
}

func (tui *compactTUI) toggleSystemPanel() {
	tui.mu.Lock()
	tui.systemPanel = !tui.systemPanel
	tui.dirty = true
	tui.mu.Unlock()
}

func (tui *compactTUI) handleModifiedKey(code, modifiers int) bool {
	if modifiers&4 == 0 {
		return false
	}
	switch code {
	case int('a'), int('A'):
		tui.toggleDetails()
	case int('p'), int('P'):
		tui.toggleCopyMode()
	case int('s'), int('S'):
		tui.toggleSystemPanel()
	case int('c'), int('C'):
		if tui.interrupt != nil {
			tui.interrupt()
		}
	default:
		return false
	}
	return true
}

func (tui *compactTUI) toggleCopyMode() {
	tui.mu.Lock()
	tui.copyMode = !tui.copyMode
	copyMode := tui.copyMode
	tui.dirty = true
	tui.mu.Unlock()
	if tui.terminal == nil {
		return
	}
	if copyMode {
		tui.render(true)
		tui.renderMu.Lock()
		_, _ = io.WriteString(tui.terminal, "\x1b[?1006l\x1b[?1000l\x1b[?25h")
		tui.renderMu.Unlock()
		return
	}
	tui.renderMu.Lock()
	_, _ = io.WriteString(tui.terminal, "\x1b[?25l\x1b[?1000h\x1b[?1006h")
	tui.renderMu.Unlock()
	tui.render(true)
}

func (tui *compactTUI) handleMouseWheel(button int) {
	if button&64 == 0 {
		return
	}
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if tui.active != nil {
		for index, task := range tui.tasks {
			if task == tui.active {
				tui.selected = index
				break
			}
		}
	}
	tui.details = true
	if button&1 == 0 {
		tui.scrollOffset += compactTUIWheelStep
		maxOffset := tui.tasks[tui.selected].logs.len()
		if tui.scrollOffset > maxOffset {
			tui.scrollOffset = maxOffset
		}
	} else if tui.scrollOffset > 0 {
		tui.scrollOffset -= compactTUIWheelStep
		if tui.scrollOffset < 0 {
			tui.scrollOffset = 0
		}
	}
	tui.scrollPaused = tui.scrollOffset > 0
	tui.dirty = true
}

func (tui *compactTUI) scrollHistory(direction int) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if len(tui.tasks) == 0 {
		return
	}
	tui.details = true
	tui.scrollOffset += direction * compactTUIWheelStep
	if tui.scrollOffset < 0 {
		tui.scrollOffset = 0
	}
	maxOffset := tui.tasks[tui.selected].logs.len()
	if tui.scrollOffset > maxOffset {
		tui.scrollOffset = maxOffset
	}
	tui.scrollPaused = tui.scrollOffset > 0
	tui.dirty = true
}

func (tui *compactTUI) setHistoryOffsetMax() {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if len(tui.tasks) == 0 {
		return
	}
	tui.details = true
	tui.scrollOffset = tui.tasks[tui.selected].logs.len()
	tui.scrollPaused = tui.scrollOffset > 0
	tui.dirty = true
}

func (tui *compactTUI) resumeLiveView() {
	tui.mu.Lock()
	tui.scrollOffset = 0
	tui.scrollPaused = false
	tui.mu.Unlock()
}

func (tui *compactTUI) moveSelection(delta int) {
	tui.mu.Lock()
	if len(tui.tasks) > 0 {
		tui.selected = (tui.selected + delta + len(tui.tasks)) % len(tui.tasks)
	}
	tui.dirty = true
	tui.mu.Unlock()
}

func (tui *compactTUI) finish(err error) {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if tui.active != nil {
		if err != nil && tui.active.status == compactTaskRunning {
			tui.active.status = compactTaskFailed
		} else if tui.active.status == compactTaskRunning {
			tui.active.status = compactTaskDone
		}
		if !tui.active.started.IsZero() {
			tui.active.duration = time.Since(tui.active.started)
		}
	}
	tui.dirty = true
}

func (tui *compactTUI) close() {
	tui.once.Do(func() { close(tui.stop) })
	<-tui.done
	tui.clearRenderedFrame()
}

func (tui *compactTUI) clearRenderedFrame() {
	if tui == nil || tui.terminal == nil {
		return
	}
	tui.renderMu.Lock()
	defer tui.renderMu.Unlock()
	tui.mu.Lock()
	tui.rendered = 0
	tui.mu.Unlock()
	_, _ = io.WriteString(tui.terminal, "\x1b[r\x1b[?1006l\x1b[?1000l\x1b[<u\x1b[?25h")
	if tui.altScreen {
		_, _ = io.WriteString(tui.terminal, "\x1b[?1049l")
		tui.altScreen = false
	}
}

func (tui *compactTUI) summaryLines() []string {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	return append([]string(nil), tui.summaries...)
}

func shouldUseCompactTUI(options Options, stdinTTY, stdoutTTY bool, term string) bool {
	return stdinTTY && stdoutTTY && term != "" && !strings.EqualFold(term, "dumb") &&
		!options.ShowCommands && !options.Plan && !options.CleanLogs && options.InitSigningKeys == ""
}

func outputLogPath(outDir string, now time.Time) string {
	return filepath.Join(outDir, "uwuCli-output_"+now.Format("20060102-150405.000000000")+".log")
}

func captureCompactOutput(reader *os.File, logFile *os.File, tui *compactTUI) error {
	defer reader.Close()
	writer := bufio.NewWriterSize(logFile, compactTUIWriteBuffer)
	buffer := make([]byte, 64*1024)
	line := make([]byte, 0, 4096)
	var writeErr error
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if writeErr == nil {
				_, writeErr = writer.Write(buffer[:count])
			}
			for _, value := range buffer[:count] {
				if value == '\n' || value == '\r' {
					if len(line) > 0 {
						tui.consume(string(line))
						line = line[:0]
					}
					continue
				}
				if len(line) < compactTUILineBytes*4 {
					line = append(line, value)
				}
			}
		}
		if readErr != nil {
			if len(line) > 0 {
				tui.consume(string(line))
			}
			if !errors.Is(readErr, io.EOF) && writeErr == nil {
				writeErr = readErr
			}
			break
		}
	}
	if err := writer.Flush(); writeErr == nil {
		writeErr = err
	}
	return writeErr
}

func waitCompactCapture(captured <-chan error, reader *os.File, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = compactTUICaptureTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-captured:
		return err
	case <-timer.C:
		_ = reader.Close()
		err := <-captured
		if err == nil {
			return fmt.Errorf("output pipe remained open after build exit")
		}
		return fmt.Errorf("output pipe remained open after build exit: %w", err)
	}
}

func redirectCompactProcessOutput(writer *os.File) (*os.File, func() error, error) {
	savedStdout, err := syscall.Dup(syscall.Stdout)
	if err != nil {
		return nil, nil, fmt.Errorf("duplicate terminal stdout: %w", err)
	}
	syscall.CloseOnExec(savedStdout)
	savedStderr, err := syscall.Dup(syscall.Stderr)
	if err != nil {
		_ = syscall.Close(savedStdout)
		return nil, nil, fmt.Errorf("duplicate terminal stderr: %w", err)
	}
	syscall.CloseOnExec(savedStderr)
	terminalFD, err := syscall.Dup(syscall.Stdout)
	if err != nil {
		_ = syscall.Close(savedStdout)
		_ = syscall.Close(savedStderr)
		return nil, nil, fmt.Errorf("duplicate terminal display: %w", err)
	}
	syscall.CloseOnExec(terminalFD)
	terminal := os.NewFile(uintptr(terminalFD), "uni-terminal")
	if err := syscall.Dup2(int(writer.Fd()), syscall.Stdout); err != nil {
		_ = terminal.Close()
		_ = syscall.Close(savedStdout)
		_ = syscall.Close(savedStderr)
		return nil, nil, fmt.Errorf("redirect process stdout: %w", err)
	}
	if err := syscall.Dup2(int(writer.Fd()), syscall.Stderr); err != nil {
		_ = syscall.Dup2(savedStdout, syscall.Stdout)
		_ = terminal.Close()
		_ = syscall.Close(savedStdout)
		_ = syscall.Close(savedStderr)
		return nil, nil, fmt.Errorf("redirect process stderr: %w", err)
	}
	var restoreOnce sync.Once
	var restoreErr error
	restore := func() error {
		restoreOnce.Do(func() {
			stdoutErr := syscall.Dup2(savedStdout, syscall.Stdout)
			stderrErr := syscall.Dup2(savedStderr, syscall.Stderr)
			_ = syscall.Close(savedStdout)
			_ = syscall.Close(savedStderr)
			restoreErr = errors.Join(stdoutErr, stderrErr)
		})
		return restoreErr
	}
	return terminal, restore, nil
}

func RunWithCompactTUI(ctx context.Context, options Options) (runErr error) {
	if !shouldUseCompactTUI(options, terminalIsTTY(os.Stdin), terminalIsTTY(os.Stdout), os.Getenv("TERM")) {
		return Run(ctx, options)
	}
	top, err := findTop()
	if err != nil {
		return Run(ctx, options)
	}
	outDir, err := outputDirectory(top)
	if err != nil {
		return Run(ctx, options)
	}
	messages := compactMessagesForLocale(compactChineseLocale())
	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, messages.fallback, err)
		return Run(ctx, options)
	}
	logPath := outputLogPath(outDir, time.Now())
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, messages.fallback, err)
		return Run(ctx, options)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		logFile.Close()
		_ = os.Remove(logPath)
		fmt.Fprintf(os.Stderr, messages.fallback, err)
		return Run(ctx, options)
	}
	terminalState, err := makeCompactTerminal(os.Stdin)
	if err != nil {
		reader.Close()
		writer.Close()
		logFile.Close()
		_ = os.Remove(logPath)
		fmt.Fprintf(os.Stderr, messages.fallback, err)
		return Run(ctx, options)
	}

	originalStdout, originalStderr := os.Stdout, os.Stderr
	tui := newCompactTUI(os.Stdin, originalStdout)
	if diskAvailable, err := readDiskAvailable(outDir); err == nil {
		tui.diskAvailable = diskAvailable
		tui.diskAvailableSet = true
	}
	if load, err := readLoadSnapshot(); err == nil {
		tui.load = load
	}
	statusServer, statusErr := startCompactStatusServer(tui)
	if statusErr == nil {
		tui.statusSocket = statusServer.socket
	} else {
		fmt.Fprintf(originalStderr, "uni: live action status unavailable: %v\n", statusErr)
	}
	terminal, restoreProcessOutput, redirectErr := redirectCompactProcessOutput(writer)
	if redirectErr != nil {
		if statusServer != nil {
			statusServer.close()
		}
		reader.Close()
		writer.Close()
		logFile.Close()
		_ = os.Remove(logPath)
		terminalState.restore()
		fmt.Fprintf(originalStderr, messages.fallback, redirectErr)
		return Run(ctx, options)
	}
	tui.terminal = terminal
	tui.interrupt = func() {
		if process, err := os.FindProcess(os.Getpid()); err == nil {
			_ = process.Signal(os.Interrupt)
		}
	}
	captured := make(chan error, 1)
	go func() { captured <- captureCompactOutput(reader, logFile, tui) }()
	os.Stdout, os.Stderr = writer, writer
	tui.start()

	defer func() {
		_ = restoreProcessOutput()
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = writer.Close()
		captureErr := waitCompactCapture(captured, reader, compactTUICaptureTimeout)
		_ = logFile.Close()
		if statusServer != nil {
			statusServer.close()
		}
		tui.finish(runErr)
		tui.render(true)
		tui.close()
		_ = terminal.Close()
		terminalState.restore()
		fmt.Fprintf(originalStdout, tui.messages.outputLog, logPath)
		for _, line := range tui.summaryLines() {
			fmt.Fprintln(originalStdout, line)
		}
		if _, pruneErr := pruneOutputLogs(outDir, automaticOutputLogLimit); pruneErr != nil {
			fmt.Fprintf(originalStderr, tui.messages.logWarning, pruneErr)
		}
		if captureErr != nil {
			fmt.Fprintf(originalStderr, tui.messages.logWarning, captureErr)
		}
	}()

	runErr = Run(withCompactTUI(ctx, tui), options)
	return runErr
}

func RunCompactTUIPreview(ctx context.Context) error {
	if !terminalIsTTY(os.Stdin) || !terminalIsTTY(os.Stdout) {
		return fmt.Errorf("--preview requires an interactive terminal")
	}
	terminalState, err := makeCompactTerminal(os.Stdin)
	if err != nil {
		return fmt.Errorf("prepare terminal for preview: %w", err)
	}
	defer terminalState.restore()

	previewContext, cancel := context.WithCancel(ctx)
	defer cancel()
	tui := newCompactTUI(os.Stdin, os.Stdout)
	tui.selected = 1
	tui.phaseStarted("graph-analysis", 24)
	tui.consume("[100% 1/1] bootstrap blueprint")
	tui.phaseFinished("graph-analysis", nil)
	tui.phaseStarted("ninja", 24)
	tui.consume("[ 63% 12640/20030 5m12s remaining] //frameworks/base:framework-minus-apex")
	tui.updateTelemetry(TelemetrySample{
		Timestamp: time.Now(), CPUPercent: 47.0, Load: loadSnapshot{one: "18.4", five: "16.8", fifteen: "14.2"},
		MemoryTotal: 128 * gibibyte, MemoryAvailable: 42 * gibibyte,
		SwapTotal: 32 * gibibyte, SwapFree: 27 * gibibyte,
		DiskAvailable: tui.diskAvailable, R8: 2, Linker: 1,
		ClangC: 2, ClangCXX: 16, Kotlinc: 3, Javac: 2, Rustc: 1,
	})
	tui.actions = []compactRunningAction{{
		id:          "preview-compile",
		description: "compile framework component",
		started:     time.Now().Add(-38 * time.Second),
	}}
	tui.actionTick = time.Now()
	tui.interrupt = cancel
	tui.start()
	<-previewContext.Done()
	tui.finish(nil)
	tui.render(true)
	tui.close()
	return nil
}
