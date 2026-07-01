//go:build resilience

// Package resilience holds the crash-safety and concurrency tests. They are
// guarded by the `resilience` build tag so the everyday `go test ./...` stays
// fast; run them with `go test -tags=resilience ./test/resilience/...`.
package resilience

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildBinary compiles the zadodb binary once per test run and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "zadodb-bin-")
		if err != nil {
			buildErr = err
			return
		}
		name := "zadodb"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		builtBin = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", builtBin, "github.com/JoaoNetoDev/zadodb/cmd/zadodb")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build failed: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("buildBinary: %v", buildErr)
	}
	return builtBin
}

// freePort returns a currently-free 127.0.0.1 address.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startServer launches the server subprocess against dir, listening on addr.
func startServer(t *testing.T, bin, dir, addr string) *exec.Cmd {
	t.Helper()
	logf, _ := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	cmd := exec.Command(bin, "serve", "--data-dir", dir, "--http-addr", addr, "--fsync", "per-commit")
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		t.Fatalf("startServer: %v", err)
	}
	// The child inherited its own handle at Start; drop the parent's copy so it
	// doesn't block TempDir cleanup after the child is killed (Windows).
	logf.Close()
	return cmd
}

// waitHealthy blocks until the server answers /v1/health or the deadline passes.
func waitHealthy(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy within %s", addr, timeout)
}

// ensureClass creates the class, tolerating an already-exists conflict.
func ensureClass(t *testing.T, addr, class string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": class})
	resp, err := http.Post("http://"+addr+"/v1/classes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ensureClass: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 409 {
		t.Fatalf("ensureClass status = %d", resp.StatusCode)
	}
}

// getObjectN fetches an object and returns its "n" marker.
func getObjectN(addr, class string, id int64) (n int64, found bool, err error) {
	resp, err := http.Get(fmt.Sprintf("http://%s/v1/classes/%s/objects/%d", addr, class, id))
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return 0, false, nil
	}
	if resp.StatusCode != 200 {
		return 0, false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var m struct {
		N int64 `json:"n"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return 0, false, err
	}
	return m.N, true, nil
}

// stopServer kills and reaps the server process.
func stopServer(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
