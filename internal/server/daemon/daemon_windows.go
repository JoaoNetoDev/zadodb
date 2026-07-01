//go:build windows

package daemon

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/JoaoNetoDev/zadodb/internal/server/config"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Serve runs as a Windows Service when launched by the SCM, otherwise in the
// foreground with Ctrl-C handling.
func Serve(cfg config.Config, logger *log.Logger) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("daemon: detect service mode: %w", err)
	}
	if isService {
		return svc.Run(ServiceName, &handler{cfg: cfg, logger: logger})
	}
	return serveForeground(cfg, logger)
}

type handler struct {
	cfg    config.Config
	logger *log.Logger
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- RunForeground(ctx, h.cfg, h.logger) }()

	s <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				<-errCh
				return false, 0
			}
		case err := <-errCh:
			if err != nil {
				h.logger.Printf("service error: %v", err)
				return true, 1
			}
			return false, 0
		}
	}
}

// Install creates the Windows service pointing at this executable with `serve`.
func Install(cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if existing, err := m.OpenService(ServiceName); err == nil {
		existing.Close()
		return fmt.Errorf("service %s already exists", ServiceName)
	}
	s, err := m.CreateService(ServiceName, exe, mgr.Config{
		DisplayName: ServiceDisplay,
		Description: "Portable object-oriented database",
		StartType:   mgr.StartAutomatic,
	}, "serve", "--data-dir", cfg.DataDir, "--http-addr", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Printf("installed service %q (start with: zadodb service start)\n", ServiceName)
	return nil
}

// Uninstall stops and removes the Windows service.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service not installed: %w", err)
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Printf("removed service %q\n", ServiceName)
	return nil
}

// Start starts the service via the SCM.
func Start() error {
	return withService(func(s *mgr.Service) error { return s.Start() })
}

// Stop signals the service to stop.
func Stop() error {
	return withService(func(s *mgr.Service) error {
		_, err := s.Control(svc.Stop)
		return err
	})
}

// Status prints the current service state.
func Status() error {
	return withService(func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		fmt.Printf("service %q state: %d\n", ServiceName, st.State)
		return nil
	})
}

func withService(fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service not installed: %w", err)
	}
	defer s.Close()
	return fn(s)
}
