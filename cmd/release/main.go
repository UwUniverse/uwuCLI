// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/uwuAOSP/uwuCLI/internal/release"
)

func main() {
	os.Exit(release.Run(os.Args[1:]))
}
