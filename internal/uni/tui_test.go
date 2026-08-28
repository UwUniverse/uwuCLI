// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
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
	if shouldUseCompactTUI(Options{NoTUI: true}, true, true, "xterm") {
		t.Fatal("--no-tui did not disable compact TUI")
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
	tui.updateTelemetry(TelemetrySample{MemoryAvailable: 13 * gibibyte, R8: 3})

	task := tui.byName["Startup"]
	if task.status != compactTaskRunning || task.jobs != 9 || task.percent != 79 || task.done != 15175 || task.total != 19054 {
		t.Fatalf("unexpected task: %+v", *task)
	}
	if tui.r8 != 3 || tui.memory != 13*gibibyte {
		t.Fatalf("unexpected telemetry: r8=%d memory=%d", tui.r8, tui.memory)
	}
	tui.phaseFinished("startup", nil)
	if task.status != compactTaskDone {
		t.Fatalf("task status=%v, want done", task.status)
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

func TestCompactMessagesAreEnglish(t *testing.T) {
	messages := englishCompactMessages()
	if messages.header != "[Task]" || messages.taskLabels["Kernel"] != "Kernel" {
		t.Fatalf("unexpected messages: %+v", messages)
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
	// VMIN=0/VTIME>0 produces an idle EOF in os.File.Read. The loop must
	// remain alive and consume input that arrives after that timeout.
	time.Sleep(150 * time.Millisecond)
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

func TestCompactTUIMouseClickSelectsTask(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.rendered = 11
	if pending := tui.handleInput([]byte("\x1b[<0;10;16M")); len(pending) != 0 {
		t.Fatalf("mouse click was not consumed: %q", pending)
	}
	if tui.selected != 1 {
		t.Fatalf("mouse click selected=%d, want 1", tui.selected)
	}
	if pending := tui.handleInput([]byte("\x1b[<2;10;17M")); len(pending) != 0 {
		t.Fatalf("right click was not consumed: %q", pending)
	}
	if !tui.details {
		t.Fatal("right click did not toggle details")
	}
	tui.details = false
	if pending := tui.handleInput([]byte{'\x1b', '[', 'M', 32, 42, 48}); len(pending) != 0 {
		t.Fatalf("legacy mouse click was not consumed: %q", pending)
	}
	if tui.selected != 1 {
		t.Fatalf("legacy mouse click selected=%d, want 1", tui.selected)
	}
}

func TestCompactTUICursorReportAnchorsMouseRows(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.rendered = 11
	tui.frameTop = 20
	tui.frameTopSet = true
	if pending := tui.handleInput([]byte("\x1b[20;1R")); len(pending) != 0 {
		t.Fatalf("cursor report was not consumed: %q", pending)
	}
	if got := <-tui.cursorReport; got != 20 {
		t.Fatalf("cursor row=%d, want 20", got)
	}
	if pending := tui.handleInput([]byte("\x1b[<0;10;21M")); len(pending) != 0 {
		t.Fatalf("mouse click was not consumed: %q", pending)
	}
	if tui.selected != 0 {
		t.Fatalf("anchored click selected=%d, want 0", tui.selected)
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
	if !strings.Contains(string(data), "\x1b[8A\x1b[1G") {
		t.Fatalf("refresh did not move to the previous frame: %q", data)
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
	second := tui.frame(true)
	if first == second {
		t.Fatalf("spinner frame did not advance: %q", first)
	}
	if !strings.Contains(first, "⠙") || !strings.Contains(second, "⠹") {
		t.Fatalf("unexpected spinner sequence: first=%q second=%q", first, second)
	}
	if strings.Contains(first, "✱") || strings.Contains(first, "●") {
		t.Fatalf("legacy running markers remain: %q", first)
	}
}

func TestCompactTUIPendingTaskUsesStableSpinner(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	first := tui.frame(true)
	second := tui.frame(true)
	if !strings.Contains(first, "◷ pending") || !strings.Contains(second, "◷ pending") {
		t.Fatalf("unexpected pending marker: first=%q second=%q", first, second)
	}
	tui.pendingAt = time.Now().Add(-time.Second)
	third := tui.frame(true)
	if !strings.Contains(third, "◶ pending") {
		t.Fatalf("pending marker did not advance slowly: %q", third)
	}
}

func TestCompactTUIProgressUsesSpinnerAndNumbers(t *testing.T) {
	tui := newCompactTUI(nil, nil)
	tui.phaseStarted("ninja", 18)
	tui.consume("[ 42% 42/100] target")
	first := tui.frame(true)
	second := tui.frame(true)
	if !strings.Contains(first, "42% 42/100") || !strings.Contains(second, "42% 42/100") {
		t.Fatalf("unexpected progress sequence: first=%q second=%q", first, second)
	}
	if !strings.Contains(first, "⠙ building") || !strings.Contains(first, "jobs=18") || strings.Contains(first, "◓ building") || strings.Contains(first, "✱") {
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
