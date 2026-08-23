// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/uwuAOSP/uwuCLI/internal/uni"
)

func main() {
	options, err := uni.ParseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "uni: %v\n", err)
		os.Exit(2)
	}
	if options.Help {
		fmt.Print(uni.Usage())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := uni.Run(ctx, options); err != nil {
		fmt.Fprintf(os.Stderr, "uni: %v\n", err)
		os.Exit(1)
	}
}
