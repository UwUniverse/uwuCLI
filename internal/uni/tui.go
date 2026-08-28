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
	"time"
	"unicode/utf8"
)

const (
	compactTUIRefreshInterval = 33 * time.Millisecond
	compactTUILogLines        = 160
	compactTUIWheelStep       = 3
	compactTUILineBytes       = 4096
	compactTUIWriteBuffer     = 256 * 1024
	compactTUICaptureTimeout  = 2 * time.Second
)

var compactTUISpinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

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
	latest   string
	logs     compactRing
}

type compactRing struct {
	lines []string
	next  int
	full  bool
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
	mu           sync.Mutex
	renderMu     sync.Mutex
	terminal     *os.File
	input        *os.File
	tasks        []*compactTask
	byName       map[string]*compactTask
	active       *compactTask
	selected     int
	details      bool
	r8           int
	memory       int64
	spinner      int
	dirty        bool
	rendered     int
	scrollPaused bool
	scrollOffset int
	summaries    []string
	messages     compactMessages

	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
	interrupt func()
}

type compactMessages struct {
	header     string
	taskLabels map[string]string
	building   string
	pending    string
	failed     string
	jobs       string
	remaining  string
	running    string
	available  string
	memory     string
	footer     string
	outputLog  string
	fallback   string
	logWarning string
}

func compactMessagesForLocale(chinese bool) compactMessages {
	if chinese {
		return compactMessages{
			header:     "[任务]",
			taskLabels: map[string]string{"Graph": "构建图", "Kernel": "内核", "Startup": "启动", "Main": "主构建"},
			building:   "编译中",
			pending:    "等待",
			failed:     "失败",
			jobs:       "并发",
			remaining:  "剩余",
			running:    "运行中",
			available:  "可用",
			memory:     "内存",
			footer:     "↑↓ 选择   Ctrl+A 详情   Ctrl+C 停止",
			outputLog:  "uni: 完整输出日志: %s\n",
			fallback:   "uni: compact TUI 已禁用，使用普通输出: %v\n",
			logWarning: "uni: 输出日志警告: %v\n",
		}
	}
	return compactMessages{
		header:     "[Task]",
		taskLabels: map[string]string{"Graph": "Graph", "Kernel": "Kernel", "Startup": "Startup", "Main": "Main"},
		building:   "building",
		pending:    "pending",
		failed:     "failed",
		jobs:       "jobs",
		remaining:  "eta",
		running:    "running",
		available:  "available",
		memory:     "RAM",
		footer:     "↑↓ Select   Ctrl+A Details   Ctrl+C Stop",
		outputLog:  "uni: output log: %s\n",
		fallback:   "uni: compact TUI disabled; using line output: %v\n",
		logWarning: "uni: output log warning: %v\n",
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

func newCompactTUI(input, terminal *os.File) *compactTUI {
	names := []string{"Graph", "Kernel", "Startup", "Main"}
	messages := compactMessagesForLocale(false)
	tui := &compactTUI{
		terminal: terminal,
		input:    input,
		byName:   make(map[string]*compactTask, len(names)),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		dirty:    true,
		messages: messages,
	}
	for _, name := range names {
		task := &compactTask{name: name, label: messages.taskLabels[name], status: compactTaskPending, logs: newCompactRing(compactTUILogLines)}
		tui.tasks = append(tui.tasks, task)
		tui.byName[name] = task
	}
	if snapshot, err := ReadMemorySnapshot(); err == nil {
		tui.memory = snapshot.Available
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
	case strings.HasPrefix(phase, "kernel"):
		return "Kernel"
	case strings.HasPrefix(phase, "startup"):
		return "Startup"
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
	tui.r8 = sample.R8
	tui.dirty = true
}

func (tui *compactTUI) consume(line string) {
	displayLine := compactDisplayLine(line)
	line = sanitizeCompactLine(line)
	if line == "" {
		return
	}
	tui.mu.Lock()
	defer tui.mu.Unlock()

	switch {
	case strings.HasPrefix(line, "uni: reuse graph"):
		task := tui.byName["Graph"]
		task.status = compactTaskDone
		task.duration = 0
		tui.active = task
	case strings.Contains(line, "Analyzing Android.bp files"):
		task := tui.byName["Graph"]
		if task.status == compactTaskPending {
			task.status = compactTaskRunning
			task.started = time.Now()
		}
		tui.active = task
	}

	if tui.active == nil {
		tui.active = tui.byName["Graph"]
	}
	if displayLine != "" {
		tui.active.latest = displayLine
	}
	tui.active.logs.add(line)
	if percent, done, total, ok := parseCompactProgress(line); ok {
		tui.active.percent = percent
		tui.active.done = done
		tui.active.total = total
	}
	if strings.HasPrefix(line, "uni: phases=") || strings.HasPrefix(line, "uni: package=") ||
		strings.HasPrefix(line, "uni: output=") || strings.HasPrefix(line, "#### build completed successfully") {
		tui.summaries = append(tui.summaries, line)
	}
	if strings.HasPrefix(line, "FAILED:") || strings.HasPrefix(line, "FAILED ") || strings.Contains(line, "ninja failed") {
		tui.summaries = append(tui.summaries, line)
	}
	tui.dirty = true
}

func compactDisplayLine(line string) string {
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

func parseCompactProgress(line string) (percent, done, total int, ok bool) {
	for offset := 0; offset < len(line); {
		open := strings.IndexByte(line[offset:], '[')
		if open < 0 {
			return 0, 0, 0, false
		}
		open += offset
		close := strings.IndexByte(line[open+1:], ']')
		if close < 0 {
			return 0, 0, 0, false
		}
		close += open + 1
		fields := strings.Fields(line[open+1 : close])
		if len(fields) >= 2 && strings.HasSuffix(fields[0], "%") {
			parsedPercent, percentErr := strconv.Atoi(strings.TrimSuffix(fields[0], "%"))
			left, right, found := strings.Cut(fields[1], "/")
			parsedDone, doneErr := strconv.Atoi(left)
			parsedTotal, totalErr := strconv.Atoi(right)
			if found && percentErr == nil && doneErr == nil && totalErr == nil && parsedTotal > 0 {
				return parsedPercent, parsedDone, parsedTotal, true
			}
		}
		offset = close + 1
	}
	return 0, 0, 0, false
}

func sanitizeCompactLine(line string) string {
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

func (tui *compactTUI) taskLine(task *compactTask) string {
	duration := task.duration
	if task.status == compactTaskRunning && !task.started.IsZero() {
		duration = time.Since(task.started)
	}
	prefix := compactPadRight(task.label, 9)
	switch task.status {
	case compactTaskDone:
		return fmt.Sprintf("%s ■ %s", prefix, compactDuration(duration))
	case compactTaskFailed:
		return fmt.Sprintf("%s × %s  %s", prefix, tui.messages.failed, compactDuration(duration))
	case compactTaskRunning:
		spinner := string(compactTUISpinner[tui.spinner%len(compactTUISpinner)])
		line := fmt.Sprintf("%s %s %s  %s  %s=%d", prefix, spinner, tui.messages.building, compactDuration(duration), tui.messages.jobs, task.jobs)
		if task.total > 0 {
			line += fmt.Sprintf("  %d%% %d/%d", task.percent, task.done, task.total)
		}
		return line
	default:
		return fmt.Sprintf("%s ○ %s", prefix, tui.messages.pending)
	}
}

func (tui *compactTUI) frame(force bool) string {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if !force && !tui.dirty {
		return ""
	}
	tui.dirty = false
	tui.spinner = (tui.spinner + 1) % len(compactTUISpinner)
	size := compactTerminalSize(tui.terminal)
	width := int(size.cols)
	if width < 40 {
		width = 40
	}
	lineWidth := width - 3
	var output strings.Builder
	output.WriteString(truncateCompactLine(tui.messages.header, lineWidth))
	output.WriteByte('\n')
	for index, task := range tui.tasks {
		marker := "  "
		if index == tui.selected {
			marker = "→ "
		}
		line := truncateCompactLine(marker+tui.taskLine(task), lineWidth)
		output.WriteString(line)
		output.WriteByte('\n')
	}
	output.WriteString(truncateCompactLine(fmt.Sprintf("  R8        %d %s", tui.r8, tui.messages.running), lineWidth))
	output.WriteByte('\n')
	output.WriteString(truncateCompactLine(fmt.Sprintf("  %s %s %s", compactPadRight(tui.messages.memory, 9), compactMemory(tui.memory), tui.messages.available), lineWidth))
	output.WriteByte('\n')
	if tui.details {
		output.WriteByte('\n')
		available := int(size.rows) - 11
		if available < 3 {
			available = 3
		}
		ring := &tui.tasks[tui.selected].logs
		lineCount := ring.len()
		if tui.scrollOffset > lineCount {
			tui.scrollOffset = lineCount
		}
		lines := ring.recent(available + tui.scrollOffset)
		end := len(lines) - tui.scrollOffset
		if end < 0 {
			end = 0
		}
		start := end - available
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:end] {
			output.WriteString(truncateCompactLine(line, lineWidth))
			output.WriteByte('\n')
		}
	}
	output.WriteByte('\n')
	output.WriteString(truncateCompactLine(tui.messages.footer, lineWidth))
	output.WriteByte('\n')
	if tui.active != nil && tui.active.latest != "" && (!tui.details || tui.scrollOffset == 0) {
		latest := truncateCompactDisplayLine(tui.active.latest, lineWidth-2)
		output.WriteString("  ")
		output.WriteString(latest)
		output.WriteByte('\n')
	}
	return output.String()
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
	prefix := ""
	if tui.rendered > 0 {
		prefix = fmt.Sprintf("\x1b[%dA\x1b[1G", tui.rendered)
	}
	var output strings.Builder
	output.Grow(len(prefix) + len(frame) + tui.rendered*4)
	output.WriteString(prefix)
	for _, line := range strings.SplitAfter(frame, "\n") {
		if line == "" {
			continue
		}
		output.WriteString("\x1b[2K\x1b[1G")
		output.WriteString(line)
	}
	output.WriteString("\x1b[J")
	_, _ = io.WriteString(tui.terminal, output.String())
	tui.mu.Lock()
	tui.rendered = strings.Count(frame, "\n")
	tui.mu.Unlock()
}

func (tui *compactTUI) animate() {
	tui.mu.Lock()
	shouldRender := tui.dirty
	if !tui.scrollPaused {
		for _, task := range tui.tasks {
			if task.status == compactTaskRunning {
				shouldRender = true
				break
			}
		}
	}
	if shouldRender {
		tui.dirty = true
	}
	tui.mu.Unlock()
	if shouldRender {
		tui.render(false)
	}
}

func (tui *compactTUI) start() {
	_, _ = io.WriteString(tui.terminal, "\x1b[>1u\x1b[?25l\x1b[?1000h\x1b[?1006h")
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
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return
			}
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
			tui.mu.Lock()
			tui.details = !tui.details
			tui.scrollOffset = 0
			tui.scrollPaused = false
			tui.dirty = true
			tui.mu.Unlock()
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
				if len(fields) >= 3 {
					button, buttonErr := strconv.Atoi(fields[0])
					x, xErr := strconv.Atoi(fields[1])
					y, yErr := strconv.Atoi(fields[2])
					if buttonErr == nil && xErr == nil && yErr == nil {
						tui.handleMouseEvent(button, x, y, input[final] == 'm')
					}
				}
				input = input[final+1:]
				continue
			}
			if input[1] == '[' && len(input) > 2 && input[2] == 'M' {
				if len(input) < 6 {
					return input
				}
				button, x, y := int(input[3])-32, int(input[4])-32, int(input[5])-32
				tui.handleMouseEvent(button, x, y, false)
				input = input[6:]
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
								tui.mu.Lock()
								tui.details = !tui.details
								tui.scrollOffset = 0
								tui.scrollPaused = false
								tui.dirty = true
								tui.mu.Unlock()
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
				code, err := strconv.Atoi(strings.Split(params, ";")[0])
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

func (tui *compactTUI) handleMouseEvent(button, _, y int, release bool) {
	if button&64 != 0 {
		tui.handleMouseWheel(button)
		return
	}
	if release || (button != 0 && button != 2) {
		return
	}
	size := compactTerminalSize(tui.terminal)
	tui.mu.Lock()
	defer tui.mu.Unlock()
	if tui.rendered <= 0 {
		return
	}
	// The rendered frame is anchored above the cursor. Coordinates are
	// one-based, while the task rows start after the header.
	top := int(size.rows) - tui.rendered
	index := y - (top + 2)
	if index < 0 || index >= len(tui.tasks) {
		return
	}
	tui.selected = index
	if button == 2 {
		tui.details = !tui.details
		tui.scrollOffset = 0
		tui.scrollPaused = false
	}
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
		if err != nil {
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
	_, _ = io.WriteString(tui.terminal, "\x1b[?1006l\x1b[?1000l\x1b[<u\x1b[?25h")
}

func (tui *compactTUI) summaryLines() []string {
	tui.mu.Lock()
	defer tui.mu.Unlock()
	return append([]string(nil), tui.summaries...)
}

func shouldUseCompactTUI(options Options, stdinTTY, stdoutTTY bool, term string) bool {
	return stdinTTY && stdoutTTY && term != "" && !strings.EqualFold(term, "dumb") &&
		!options.NoTUI && !options.ShowCommands && !options.Plan && !options.CleanLogs
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
	messages := compactMessagesForLocale(false)
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
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = writer.Close()
		captureErr := waitCompactCapture(captured, reader, compactTUICaptureTimeout)
		_ = logFile.Close()
		tui.finish(runErr)
		tui.render(true)
		tui.close()
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
