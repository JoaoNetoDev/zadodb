package main

import (
	"fmt"
	"runtime"
)

// Build metadata. Overridable at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X main.version=v0.1.0 -X main.commit=abc123"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func runVersion() {
	fmt.Printf("zadodb %s (commit %s, built %s, %s/%s, %s)\n",
		version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
