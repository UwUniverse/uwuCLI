// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import "testing"

func TestClassifyBuildProcess(t *testing.T) {
	tests := map[string]string{
		"/tool/clang++\x00-c\x00a.cc\x00":                                   "clang",
		"/tool/ld.lld\x00-o\x00lib.so\x00":                                  "linker",
		"java\x00com.android.tools.r8.R8\x00":                               "r8",
		"java\x00org.jetbrains.kotlin.cli.jvm.K2JVMCompiler\x00kotlinc\x00": "kotlinc",
		"/tool/rustc\x00crate.rs\x00":                                       "rustc",
	}
	for command, want := range tests {
		_, got := classifyBuildProcess([]byte(command), "")
		if got != want {
			t.Fatalf("classify %q = %q, want %q", command, got, want)
		}
	}
}

func TestClassifyBuildProcessDoesNotCountShellWrappers(t *testing.T) {
	for _, command := range []string{
		"/bin/bash\x00-c\x00java -jar kotlin-compiler.jar\x00",
		"/bin/bash\x00-c\x00rustc crate.rs\x00",
		"/bin/sh\x00-c\x00clang++ -c source.cc\x00",
	} {
		_, got := classifyBuildProcess([]byte(command), "")
		if got != "other" {
			t.Fatalf("classify wrapper %q = %q, want other", command, got)
		}
	}
}
