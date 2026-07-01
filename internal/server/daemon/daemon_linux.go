//go:build !windows

package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/JoaoNetoDev/zadodb/internal/server/config"
)

const systemdUnitPath = "/etc/systemd/system/zadodb.service"

// Serve runs the server in the foreground (systemd supervises it as
// Type=simple).
func Serve(cfg config.Config, logger *log.Logger) error {
	return serveForeground(cfg, logger)
}

// Install writes a systemd unit that launches this binary with `serve` and
// reloads systemd. Requires root.
func Install(cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dataDir := cfg.DataDir
	unit := fmt.Sprintf(`[Unit]
Description=%s
After=network.target

[Service]
Type=simple
ExecStart=%s serve --data-dir %q --http-addr %q
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`, ServiceDisplay, exe, dataDir, cfg.HTTPAddr)

	if err := os.WriteFile(systemdUnitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("daemon: write unit (need root?): %w", err)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Printf("installed %s\n", systemdUnitPath)
	fmt.Printf("enable + start with: sudo systemctl enable --now %s\n", ServiceName)
	return nil
}

// Uninstall stops, disables, and removes the systemd unit.
func Uninstall() error {
	_ = run("systemctl", "stop", ServiceName)
	_ = run("systemctl", "disable", ServiceName)
	if err := os.Remove(systemdUnitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return run("systemctl", "daemon-reload")
}

// Start starts the service via systemd.
func Start() error { return run("systemctl", "start", ServiceName) }

// Stop stops the service via systemd.
func Stop() error { return run("systemctl", "stop", ServiceName) }

// Status prints the service status via systemd.
func Status() error { return run("systemctl", "status", ServiceName) }

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("daemon: %s %v: %w", name, args, err)
	}
	return nil
}
