// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type compactTerminalState struct {
	fd       int
	original syscall.Termios
}

type terminalSize struct {
	rows uint16
	cols uint16
}

func terminalIsTTY(file *os.File) bool {
	if file == nil {
		return false
	}
	var state syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&state)))
	return errno == 0
}

func makeCompactTerminal(file *os.File) (*compactTerminalState, error) {
	if file == nil {
		return nil, fmt.Errorf("terminal input is unavailable")
	}
	fd := int(file.Fd())
	var original syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&original))); errno != 0 {
		return nil, errno
	}
	configured := original
	configured.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.IXOFF | syscall.ISTRIP
	configured.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.IEXTEN
	configured.Cflag |= syscall.CS8
	configured.Cc[syscall.VMIN] = 0
	configured.Cc[syscall.VTIME] = 1
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&configured))); errno != 0 {
		return nil, errno
	}
	return &compactTerminalState{fd: fd, original: original}, nil
}

func (state *compactTerminalState) restore() {
	if state == nil {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(state.fd), syscall.TCSETS, uintptr(unsafe.Pointer(&state.original)))
}

func compactTerminalSize(file *os.File) terminalSize {
	size := terminalSize{rows: 24, cols: 100}
	if file == nil {
		return size
	}
	var value struct {
		rows   uint16
		cols   uint16
		xpixel uint16
		ypixel uint16
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&value))); errno == 0 {
		if value.rows > 0 {
			size.rows = value.rows
		}
		if value.cols > 0 {
			size.cols = value.cols
		}
	}
	return size
}
