// Package integration exercises the full server binary end to end over real
// HTTP, including a restart to prove data persists across process lifetimes.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	name := "zadodb"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	out, err := exec.Command("go", "build", "-o", bin, "github.com/JoaoNetoDev/zadodb/cmd/zadodb").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

func start(t *testing.T, bin, dir, addr string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "serve", "--data-dir", dir, "--http-addr", addr)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/v1/health"); err == nil {
			resp.Body.Close()
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not start")
	return nil
}

func req(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	rq, _ := http.NewRequest(method, url, r)
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		json.Unmarshal(data, &m)
	}
	return resp.StatusCode, m
}

func TestEndToEndWithRestart(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	addr := freeAddr(t)

	cmd := start(t, bin, dir, addr)
	base := "http://" + addr

	if code, _ := req(t, "POST", base+"/v1/classes", map[string]any{"name": "Pessoa"}); code != 201 {
		t.Fatalf("create class: %d", code)
	}
	for i := 0; i < 25; i++ {
		code, m := req(t, "POST", base+"/v1/classes/Pessoa/objects", map[string]any{"n": i})
		if code != 201 {
			t.Fatalf("create object %d: %d", i, code)
		}
		if int(m["id"].(float64)) != i+1 {
			t.Fatalf("object %d got id %v", i, m["id"])
		}
	}

	// Graceful stop, then restart and confirm everything is still there.
	cmd.Process.Signal(os.Interrupt)
	if runtime.GOOS == "windows" {
		cmd.Process.Kill() // Interrupt isn't delivered on Windows; hard stop still recovers
	}
	cmd.Wait()

	cmd2 := start(t, bin, dir, addr)
	defer func() { cmd2.Process.Kill(); cmd2.Wait() }()

	code, m := req(t, "GET", base+"/v1/classes/Pessoa/objects?limit=100", nil)
	if code != 200 {
		t.Fatalf("list after restart: %d", code)
	}
	if int(m["count"].(float64)) != 25 {
		t.Fatalf("count after restart = %v, want 25", m["count"])
	}
	// Spot-check a specific object's payload survived intact.
	code, obj := req(t, "GET", fmt.Sprintf("%s/v1/classes/Pessoa/objects/13", base), nil)
	if code != 200 || int(obj["n"].(float64)) != 12 {
		t.Fatalf("object 13 after restart = %d %v", code, obj)
	}
}
