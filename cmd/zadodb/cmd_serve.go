package main

import "errors"

// runServe is wired up to the config + engine + HTTP server in a later step.
// It is declared here so the command dispatch in main.go compiles from the
// first bootstrap commit.
func runServe(args []string) error {
	return errors.New("serve: not implemented yet")
}
