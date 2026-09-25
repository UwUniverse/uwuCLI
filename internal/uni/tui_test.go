// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShouldUseCompactTUI(t *testing.T) {
	if !shouldUseCompactTUI(Options{}, true, true, "xterm-256color") {
		t.Fatal("interactive terminal should enable compact TUI")
	}
	for name, options := range map[string]Options{
		"show commands": {ShowCommands: true},
		"plan":          {Plan: true},
		"clean logs":    {CleanLogs: true},
	} {
		t.Run(name, func(t *testing.T) {
			if shouldUseCompactTUI(options, true, true, "xterm") {
				t.Fatal("non-build output mode should use line output")
			}
		})
	}
	if shouldUseCompactTUI(Options{}, false, true, "xterm") ||
		shouldUseCompactTUI(Options{}, true, false, "xterm") ||
		shouldUseCompactTUI(Options{}, true, true, "dumb") {
		t.Fatal("non-interactive terminal should use line output")
	}
}

func TestParseCompactProgress(t *testing.T) {
	percent, done, total, ok := parseCompactProgress("[ 68% 156383/229745] //module:target r8 [common]")
	if !ok || percent != 68 || done != 156383 || total != 229745 {
		t.Fatalf("unexpected progress: %d %d/%d ok=%t", percent, done, total, ok)
	}
	if _, _, _, ok := parseCompactProgress("ordinary compiler output"); ok {
		t.Fatal("ordinary output must not be parsed as progress")
	}
}

func TestCompactRingIsBoundedAndOrdered(t *testing.T) {
	ring := newCompactRing(3)
	for _, line := range []string{"one", "two", "three", "four"} {
		ring.add(line)
	}
	if got := strings.Join(ring.recent(10), ","); got != "two,three,four" {
		t.Fatalf("unexpected ring contents: %q", got)
	}
	if got := strings.Join(ring.recent(2), ","); got != "three,four" {
		t.Fatalf("unexpected recent contents: %q", got)
	}
}

func TestSanitizeCompactLine(t *testing.T) {
	line := sanitizeCompactLine("\x1b[31mFAILED:\x1b[0m\tmodule\x07")
	if line != "FAILED:    module" {
		t.Fatalf("unexpected sanitized line: %q", line)
	}
}

func TestCompactDisplayLinePreservesColorsAndProgress(t *testing.T) {
	line := compactDisplayLine("\x1b[32m[100% 1/1] bootstrap blueprint\x1b[0m")
	if !strings.Contains(line, "\x1b[32m") || !strings.Contains(line, "[100% 1/1]") || !strings.Contains(line, "\x1b[0m") {
		t.Fatalf("display line lost color or progress: %q", line)
	}
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("graph-analysis", 12)
	tui.consume("\x1b[32m[100% 1/1] bootstrap blueprint\x1b[0m")
	frame := tui.frame(true)
	if !strings.Contains(frame, "\x1b[32m[100% 1/1] bootstrap blueprint\x1b[0m") {
		t.Fatalf("frame lost colored progress line: %q", frame)
	}
	if !strings.HasSuffix(frame, "\x1b[0m") {
		t.Fatalf("latest output is not the final TUI line: %q", frame)
	}
}

func TestCompactTUIR8Status(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	idle := tui.frame(true)
	if !strings.Contains(idle, "R8        □ idle") || strings.Contains(idle, "0 running") {
		t.Fatalf("idle R8 status is misleading: %q", idle)
	}

	tui.updateTelemetry(TelemetrySample{R8: 3})
	active := tui.frame(true)
	if !strings.Contains(active, "R8        3 running") {
		t.Fatalf("active R8 status is missing: %q", active)
	}
}

func TestCompactDisplayLineIsBoundedForRedraw(t *testing.T) {
	line := "\x1b[31m[ 96% 302/312] very-long-module-name-that-would-wrap-on-a-narrow-terminal\x1b[0m"
	if got := truncateCompactDisplayLine(line, 24); compactTextWidth(sanitizeCompactLine(got)) > 24 {
		t.Fatalf("display line exceeds redraw width: %q", got)
	}
}

func TestCompactTUITracksPhaseProgressAndTelemetry(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("startup", 9)
	tui.consume("[ 79% 15175/19054] //module:target r8 [common]")
	tui.updateTelemetry(TelemetrySample{
		MemoryAvailable: 13 * gibibyte,
		SwapTotal:       8 * gibibyte,
		SwapFree:        3 * gibibyte,
		SwapOutBytes:    2 * gibibyte,
		Linker:          1,
	})

	task := tui.byName["Startup"]
	if task.status != compactTaskRunning || task.jobs != 9 || task.percent != 79 || task.done != 15175 || task.total != 19054 {
		t.Fatalf("unexpected task: %+v", *task)
	}
	if tui.r8 != 0 || tui.memory != 13*gibibyte {
		t.Fatalf("unexpected telemetry: r8=%d memory=%d", tui.r8, tui.memory)
	}
	if tui.swapTotal != 8*gibibyte || tui.swapFree != 3*gibibyte || tui.swapOut != 2*gibibyte {
		t.Fatalf("unexpected swap telemetry: total=%d free=%d out=%d", tui.swapTotal, tui.swapFree, tui.swapOut)
	}
	if task.activity != "link=1" || !strings.Contains(tui.taskLine(task), "link=1") {
		t.Fatalf("active linker is not visible: task=%+v line=%q", *task, tui.taskLine(task))
	}
	tui.phaseFinished("startup", nil)
	if task.status != compactTaskDone {
		t.Fatalf("task status=%v, want done", task.status)
	}
}

func TestCompactActivityUsesCurrentCompilerClass(t *testing.T) {
	tests := []struct {
		name   string
		sample TelemetrySample
		want   string
	}{
		{name: "r8", sample: TelemetrySample{R8: 2, Linker: 1}, want: "R8=2"},
		{name: "linker", sample: TelemetrySample{Linker: 1, Clang: 8}, want: "link=1"},
		{name: "kotlin", sample: TelemetrySample{Kotlinc: 3}, want: "Kotlin=3"},
		{name: "java", sample: TelemetrySample{Javac: 7}, want: "Java=7"},
		{name: "rust", sample: TelemetrySample{Rustc: 10}, want: "Rust=10"},
		{name: "clang", sample: TelemetrySample{Clang: 12}, want: "C/C++=12"},
		{name: "idle", sample: TelemetrySample{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := compactActivity(test.sample); got != test.want {
				t.Fatalf("activity=%q, want %q", got, test.want)
			}
		})
	}
}

func TestCompactTUIDelayedGraphOutputDoesNotStealMainPhase(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("ninja", 18)
	tui.consume("uni: reuse graph")
	tui.consume("[ 42% 42/100] target")

	main := tui.byName["Main"]
	if tui.active != main || main.percent != 42 || main.logs.len() != 1 {
		t.Fatalf("delayed graph output stole main phase: active=%v main=%+v", tui.active, main)
	}
	if graph := tui.byName["Graph"]; graph.status != compactTaskDone || graph.logs.len() != 1 {
		t.Fatalf("graph marker was not retained: %+v", graph)
	}
	tui.selected = 3
	tui.toggleDetails()
	if frame := tui.frame(true); !strings.Contains(frame, "[ 42% 42/100] target") {
		t.Fatalf("main details are missing: %q", frame)
	}
	tui.phaseFinished("ninja", nil)
	if line := tui.taskLine(main); !strings.Contains(line, "42% 42/100") {
		t.Fatalf("completed phase lost progress: %q", line)
	}
}

func TestCompactTUIFinishPreservesCompletedPhase(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("kernel", 18)
	tui.phaseFinished("kernel", nil)
	tui.finish(errors.New("scheduler failed after kernel"))
	if status := tui.byName["Kernel"].status; status != compactTaskDone {
		t.Fatalf("completed kernel status = %v, want done", status)
	}
}

func TestCompactTUIClearRenderedFrameRemovesDashboard(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(nil, writer)
	tui.render(true)
	rendered := tui.rendered
	tui.clearRenderedFrame()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	want := "\r\x1b[J"
	if rendered > 1 {
		want = fmt.Sprintf("\r\x1b[%dA\x1b[J", rendered-1)
	}
	if !strings.HasSuffix(string(data), want) {
		t.Fatalf("dashboard was not cleared: rendered=%d output=%q", rendered, data)
	}
	if tui.rendered != 0 {
		t.Fatalf("rendered lines=%d, want 0", tui.rendered)
	}
}

func TestCompactTUISuccessSummaryKeepsColor(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.consume("\x1b[0;32m#### build completed successfully (01:17:07 (hh:mm:ss)) ####\x1b[0m")
	lines := tui.summaryLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "\x1b[0;32m") || !strings.Contains(lines[0], "\x1b[0m") {
		t.Fatalf("success summary lost color: %q", lines)
	}
}

func TestCaptureCompactOutputPreservesRawBytes(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "output.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(nil, nil)
	done := make(chan error, 1)
	go func() { done <- captureCompactOutput(reader, logFile, tui) }()
	raw := []byte("\x1b[32m[ 42% 42/100] target\x1b[0m\nuni: output=/tmp/product\n")
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(raw) {
		t.Fatalf("raw output changed:\n got %q\nwant %q", stored, raw)
	}
	if task := tui.byName["Graph"]; task.percent != 42 || task.done != 42 || task.total != 100 {
		t.Fatalf("progress not captured: %+v", *task)
	}
	if got := strings.Join(tui.summaryLines(), "\n"); got != "uni: output=/tmp/product" {
		t.Fatalf("unexpected summaries: %q", got)
	}
}

func TestOutputLogPathSortsByTimestamp(t *testing.T) {
	first := outputLogPath("/tmp/out", time.Date(2026, 8, 28, 1, 2, 3, 4, time.Local))
	second := outputLogPath("/tmp/out", time.Date(2026, 8, 28, 1, 2, 4, 0, time.Local))
	if first >= second {
		t.Fatalf("paths are not chronological: %q >= %q", first, second)
	}
}

func TestCompactMessagesSupportChineseAndEnglish(t *testing.T) {
	english := compactMessagesForLocale(false)
	chinese := compactMessagesForLocale(true)
	if english.header != "[Task]" || english.taskLabels["Kernel"] != "Kernel" {
		t.Fatalf("unexpected English messages: %+v", english)
	}
	if chinese.header != "[任务]" || chinese.taskLabels["Kernel"] != "内核" || chinese.footer == english.footer {
		t.Fatalf("unexpected Chinese messages: %+v", chinese)
	}
	if compactTextWidth(compactPadRight("内核", 9)) != 9 {
		t.Fatal("Chinese task label is not aligned to terminal columns")
	}
}

func TestCompactTUIHandlesApplicationCursorKeysAndViKeys(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	for _, input := range [][]byte{[]byte("\x1bOA"), []byte("j"), []byte("\x1b[1;2B")} {
		if pending := tui.handleInput(input); len(pending) != 0 {
			t.Fatalf("unconsumed input %q: %q", input, pending)
		}
	}
	if tui.selected != 1 {
		t.Fatalf("selection=%d, want 1", tui.selected)
	}
	if pending := tui.handleInput([]byte("k")); len(pending) != 0 || tui.selected != 0 {
		t.Fatalf("k did not move selection: selected=%d pending=%q", tui.selected, pending)
	}
}

func TestCompactTUIInputLoopConsumesTerminalBytes(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(reader, nil)
	inputDone := make(chan struct{})
	go func() {
		tui.inputLoop()
		close(inputDone)
	}()
	if _, err := writer.Write([]byte{0x01}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		tui.mu.Lock()
		details := tui.details
		tui.mu.Unlock()
		if details {
			break
		}
		select {
		case <-deadline:
			t.Fatal("input loop did not consume Ctrl+A")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(tui.stop)
	_ = writer.Close()
	_ = reader.Close()
	select {
	case <-inputDone:
	case <-time.After(time.Second):
		t.Fatal("input loop did not stop")
	}
}

func TestCompactTUISelectionUsesArrowWithoutReverseVideo(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	first := tui.frame(true)
	if !strings.Contains(first, "→ "+tui.tasks[0].label) || strings.Contains(first, "\x1b[7m") {
		t.Fatalf("unexpected initial selection rendering: %q", first)
	}
	if pending := tui.handleInput([]byte("\x1b[B")); len(pending) != 0 {
		t.Fatalf("unconsumed input: %q", pending)
	}
	second := tui.frame(true)
	if !strings.Contains(second, "→ "+tui.tasks[1].label) || strings.Contains(second, "\x1b[7m") {
		t.Fatalf("unexpected moved selection rendering: %q", second)
	}
}

func TestCompactTUIFrameDoesNotClearExistingTerminalOutput(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	frame := tui.frame(true)
	if strings.Contains(frame, "\x1b[2J") || strings.Contains(frame, "\x1b[H") {
		t.Fatalf("frame clears output printed before uni: %q", frame)
	}
	if strings.HasSuffix(frame, "\n") {
		t.Fatalf("inline frame must keep the cursor on its final row: %q", frame)
	}
}

func TestCompactTUIRenderDoesNotScrollOnRefresh(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(nil, writer)
	tui.render(true)
	tui.render(true)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\x1b[10A\x1b[1G") {
		t.Fatalf("refresh did not return to the previous frame: %q", data)
	}
}

func TestCompactTUIFrameKeepsStableHeightWithoutLatestOutput(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	withoutLatest := tui.frame(true)
	tui.consume("build output")
	withLatest := tui.frame(true)
	if compactFrameLines(withoutLatest) != compactFrameLines(withLatest) {
		t.Fatalf("frame height changed when latest output appeared: before=%d after=%d", compactFrameLines(withoutLatest), compactFrameLines(withLatest))
	}
}

func TestCompactTUIDetailsFitTerminalHeight(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("startup", 18)
	for i := 0; i < 100; i++ {
		tui.consume("build output line")
	}
	tui.toggleDetails()
	frame := tui.frame(true)
	if lines := compactFrameLines(frame); lines > 24 {
		t.Fatalf("details frame exceeds terminal height: lines=%d frame=%q", lines, frame)
	}
	if !strings.HasPrefix(frame, "[Task]\n") {
		t.Fatalf("task header was pushed out of details frame: %q", frame)
	}
}

func TestCompactTUIFrameReservesTerminalColumn(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("graph-analysis", 18)
	tui.consume("[ 96% 302/312] " + strings.Repeat("module/", 30))
	frame := tui.frame(true)
	for _, line := range strings.Split(strings.TrimSuffix(frame, "\n"), "\n") {
		if compactTextWidth(sanitizeCompactLine(line)) >= 100 {
			t.Fatalf("frame line can wrap at terminal width: %q", line)
		}
	}
}

func TestCompactTUIRunningTaskUsesSpinner(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("startup", 4)
	first := tui.frame(true)
	tui.spinnerAt = time.Now().Add(-compactTUISpinnerInterval)
	tui.animate()
	second := tui.frame(true)
	if first == second {
		t.Fatalf("spinner frame did not advance: %q", first)
	}
	if !strings.Contains(first, "|") || !strings.Contains(second, "/") {
		t.Fatalf("unexpected spinner sequence: first=%q second=%q", first, second)
	}
	if strings.Contains(first, "✱") || strings.Contains(first, "●") {
		t.Fatalf("legacy running markers remain: %q", first)
	}
}

func TestCompactTUIAnimationIsRateLimited(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("startup", 4)
	tui.frame(true)

	spinner := tui.spinner
	tui.animate()
	if tui.spinner != spinner || tui.dirty {
		t.Fatalf("animation advanced before interval: spinner=%d dirty=%t", tui.spinner, tui.dirty)
	}

	tui.spinnerAt = time.Now().Add(-compactTUISpinnerInterval)
	tui.animate()
	if tui.spinner == spinner {
		t.Fatal("animation did not advance after interval")
	}
}

func TestCompactTUIPendingTaskUsesHollowSquare(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	frame := tui.frame(true)
	if !strings.Contains(frame, "□ pending") {
		t.Fatalf("unexpected pending marker: %q", frame)
	}
	if strings.Contains(frame, "🌒") || strings.Contains(frame, "🌓") {
		t.Fatalf("pending task must not use moon phases: %q", frame)
	}
}

func TestCompactTUIProgressUsesSpinnerAndNumbers(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("ninja", 18)
	tui.consume("[ 42% 42/100] target")
	first := tui.frame(true)
	tui.spinnerAt = time.Now().Add(-compactTUISpinnerInterval)
	tui.animate()
	second := tui.frame(true)
	if !strings.Contains(first, "42% 42/100") || !strings.Contains(second, "42% 42/100") {
		t.Fatalf("unexpected progress sequence: first=%q second=%q", first, second)
	}
	if !strings.Contains(first, "| building") || !strings.Contains(first, "jobs=18") || strings.Contains(first, "🌒") || strings.Contains(first, "✱") {
		t.Fatalf("progress line is missing execution details: first=%q", first)
	}
}

func TestCompactTUIHandlesCtrlA(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	if pending := tui.handleInput([]byte{0x01}); len(pending) != 0 {
		t.Fatalf("Ctrl+A was not consumed: %q", pending)
	}
	if !tui.details {
		t.Fatal("Ctrl+A did not enable details")
	}
	if pending := tui.handleInput([]byte{0x01}); len(pending) != 0 {
		t.Fatalf("second Ctrl+A was not consumed: %q", pending)
	}
	if tui.details {
		t.Fatal("second Ctrl+A did not disable details")
	}
	if pending := tui.handleInput([]byte("\x1b[97;5u")); len(pending) != 0 {
		t.Fatalf("Kitty Ctrl+A was not consumed: %q", pending)
	}
	if !tui.details {
		t.Fatal("Kitty Ctrl+A did not enable details")
	}
	if pending := tui.handleInput([]byte("\x1b[27;5;97~")); len(pending) != 0 {
		t.Fatalf("xterm Ctrl+A was not consumed: %q", pending)
	}
	if tui.details {
		t.Fatal("xterm Ctrl+A did not disable details")
	}
}

func TestCompactTUIHandlesCtrlPCopyMode(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(nil, writer)
	if pending := tui.handleInput([]byte{0x10}); len(pending) != 0 {
		t.Fatalf("Ctrl+P was not consumed: %q", pending)
	}
	if !tui.copyMode {
		t.Fatal("Ctrl+P did not enable copy mode")
	}
	if frame := tui.frame(true); !strings.Contains(frame, "Ctrl+P Resume") {
		t.Fatalf("copy mode footer is missing: %q", frame)
	}
	if pending := tui.handleInput([]byte("\x1b[112;5u")); len(pending) != 0 {
		t.Fatalf("Kitty Ctrl+P was not consumed: %q", pending)
	}
	if tui.copyMode {
		t.Fatal("Kitty Ctrl+P did not disable copy mode")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\x1b[?1006l\x1b[?1000l\x1b[?25h") ||
		!strings.Contains(string(data), "\x1b[?25l\x1b[?1000h\x1b[?1006h") {
		t.Fatalf("copy mode did not release and restore the mouse: %q", data)
	}
}

func TestCompactTUIMouseWheelPausesAndResumes(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("graph-analysis", 18)
	for i := 0; i < 12; i++ {
		tui.consume("build output line")
	}
	if pending := tui.handleInput([]byte("\x1b[<64;10;10M")); len(pending) != 0 {
		t.Fatalf("wheel-up sequence was not consumed: %q", pending)
	}
	if !tui.scrollPaused {
		t.Fatal("wheel up did not pause redraw")
	}
	if !tui.details || tui.scrollOffset == 0 {
		t.Fatalf("wheel up did not open history view: details=%t offset=%d", tui.details, tui.scrollOffset)
	}
	if pending := tui.handleInput([]byte("\x1b[<65;10;10M")); len(pending) != 0 {
		t.Fatalf("wheel-down sequence was not consumed: %q", pending)
	}
	if tui.scrollPaused {
		t.Fatal("wheel down did not resume redraw at live position")
	}
	if pending := tui.handleInput([]byte("\x1b[64;10;10M")); len(pending) != 0 {
		t.Fatalf("legacy wheel-up sequence was not consumed: %q", pending)
	}
	if !tui.scrollPaused {
		t.Fatal("legacy wheel up did not pause redraw")
	}
}

func TestCaptureCompactOutputDrainsAfterLogFailure(t *testing.T) {
	logFile, err := os.Create(filepath.Join(t.TempDir(), "closed.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tui := newCompactTUI(nil, nil)
	done := make(chan error, 1)
	go func() { done <- captureCompactOutput(reader, logFile, tui) }()
	payload := strings.Repeat("compiler output\n", 50000) + "uni: output=/tmp/final\n"
	if _, err := writer.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("closed log file should report a write error")
	}
	if got := strings.Join(tui.summaryLines(), "\n"); got != "uni: output=/tmp/final" {
		t.Fatalf("capture stopped draining after write failure: %q", got)
	}
}

func TestWaitCompactCaptureCannotHangOnInheritedWriter(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	captured := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := reader.Read(buffer)
		captured <- err
	}()
	started := time.Now()
	err = waitCompactCapture(captured, reader, 20*time.Millisecond)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("wait did not stop promptly: elapsed=%s err=%v", time.Since(started), err)
	}
}

func BenchmarkCompactTUIConsume(b *testing.B) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("ninja", 18)
	line := "[ 68% 156383/229745] //external/protobuf:libprotobuf-cpp-lite clang++ coded_stream.cc [arm apex1000]"
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		tui.consume(line)
	}
}
