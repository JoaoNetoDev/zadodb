//go:build resilience

package resilience

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/testutil/killer"
	"github.com/JoaoNetoDev/zadodb/internal/testutil/loadgen"
)

// TestSIGKILLFuzz repeatedly hammers the server with concurrent writes and hard-
// kills it at a random moment, then restarts it. After all cycles it verifies
// that EVERY acknowledged write (HTTP 201 -> fsynced) survived intact. Writes
// that were never acknowledged may be absent — that is allowed. A missing or
// mismatched acknowledged write means data loss or corruption: a hard failure.
func TestSIGKILLFuzz(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))

	const cycles = 20
	var allAcks []loadgen.Ack

	for c := 0; c < cycles; c++ {
		addr := freePort(t)
		cmd := startServer(t, bin, dir, addr)
		waitHealthy(t, addr, 15*time.Second)
		ensureClass(t, addr, "Load")

		ctx, cancel := context.WithCancel(context.Background())
		res := loadgen.Run(ctx, "http://"+addr, "Load", 16)

		// Let load run for a random short window, then kill mid-flight.
		time.Sleep(time.Duration(50+rng.Intn(350)) * time.Millisecond)
		if err := killer.Kill(cmd.Process); err != nil {
			t.Fatalf("kill: %v", err)
		}
		cancel()
		res.Wait()
		cmd.Wait()

		acks := res.Acks()
		allAcks = append(allAcks, acks...)
		t.Logf("cycle %2d: %4d acked this cycle, %5d total", c, len(acks), len(allAcks))
	}

	if len(allAcks) == 0 {
		t.Fatal("no acknowledged writes recorded; harness produced no load")
	}

	// Final restart: verify all acknowledged writes survived the crashes.
	addr := freePort(t)
	cmd := startServer(t, bin, dir, addr)
	waitHealthy(t, addr, 15*time.Second)
	defer stopServer(cmd)

	missing, mismatch := 0, 0
	for _, a := range allAcks {
		n, found, err := getObjectN(addr, "Load", a.ID)
		if err != nil {
			t.Fatalf("verify GET id=%d: %v", a.ID, err)
		}
		if !found {
			missing++
			continue
		}
		if n != a.N {
			mismatch++
		}
	}
	if missing > 0 || mismatch > 0 {
		t.Fatalf("data loss/corruption: %d/%d acknowledged writes missing, %d mismatched",
			missing, len(allAcks), mismatch)
	}
	t.Logf("PASS: all %d acknowledged writes survived %d hard kills, none corrupted",
		len(allAcks), cycles)
}
