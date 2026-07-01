package main

import "errors"

// runService manages the OS-level service (Windows Service / systemd unit).
// Implemented in a later step; declared here so main.go compiles.
func runService(args []string) error {
	return errors.New("service: not implemented yet")
}
