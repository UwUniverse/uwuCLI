// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/uwuAOSP/uwuCLI/internal/repo"
)

func main() {
	if err := repo.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
}
