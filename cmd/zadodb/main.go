// Command zadodb is the single-binary entrypoint for the ZadoDB server.
//
// ZadoDB is a portable, object-oriented database engine focused on real
// concurrent writes, crash-safety (survives kill -9 without corruption),
// atomic writes, and fast mmap-backed reads. It exposes a REST/HTTP API so
// any language can consume it.
//
// Usage:
//
//	zadodb serve   [flags]      run the server in the foreground
//	zadodb service <action>     manage the OS service (install/uninstall/...)
//	zadodb version              print version information
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "service":
		err = runService(args)
	case "version", "-v", "--version":
		runVersion()
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "zadodb: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "zadodb: %v\n", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `zadodb - portable object-oriented database

Usage:
  zadodb <command> [flags]

Commands:
  serve      Run the database server in the foreground
  service    Manage the OS service (install|uninstall|start|stop|status)
  version    Print version information
  help       Show this help

Run "zadodb <command> -h" for command-specific flags.
`)
}
