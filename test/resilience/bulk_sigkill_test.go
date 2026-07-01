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

// TestBulkSIGKILLFuzz hammers the bulk endpoint with concurrent batch inserts
// and hard-kills the server at random moments. After all cycles it verifies
// that every acknowledged bulk survived ENTIRELY and intact — proving both the
// durability contract and batch atomicity (a bulk is all-or-nothing, so an
// acknowledged one is never partially present).
func TestBulkSIGKILLFuzz(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(7))

	const cycles = 15
	const batchSize = 50
	var allBulks []loadgen.BulkAck

	for c := 0; c < cycles; c++ {
		addr := freePort(t)
		cmd := startServer(t, bin, dir, addr)
		waitHealthy(t, addr, 15*time.Second)
		ensureClass(t, addr, "Bulk")

		ctx, cancel := context.WithCancel(context.Background())
		res := loadgen.RunBulk(ctx, "http://"+addr, "Bulk", 8, batchSize)

		time.Sleep(time.Duration(60+rng.Intn(340)) * time.Millisecond)
		if err := killer.Kill(cmd.Process); err != nil {
			t.Fatalf("kill: %v", err)
		}
		cancel()
		res.Wait()
		cmd.Wait()

		bulks := res.Bulks()
		allBulks = append(allBulks, bulks...)
		t.Logf("cycle %2d: %4d acked bulks this cycle, %5d total (%d objects)",
			c, len(bulks), len(allBulks), len(allBulks)*batchSize)
	}

	if len(allBulks) == 0 {
		t.Fatal("no acknowledged bulks recorded")
	}

	// Final restart: every acknowledged bulk must be fully present and correct.
	addr := freePort(t)
	cmd := startServer(t, bin, dir, addr)
	waitHealthy(t, addr, 15*time.Second)
	defer stopServer(cmd)

	partial, missing, mismatch := 0, 0, 0
	for _, bulk := range allBulks {
		present := 0
		for i, id := range bulk.IDs {
			n, found, err := getObjectN(addr, "Bulk", id)
			if err != nil {
				t.Fatalf("verify GET id=%d: %v", id, err)
			}
			if !found {
				continue
			}
			present++
			if n != bulk.Ns[i] {
				mismatch++
			}
		}
		switch present {
		case 0:
			missing++ // whole acked bulk gone — durability violation
		case len(bulk.IDs):
			// fully present: correct
		default:
			partial++ // atomicity violation: a bulk applied partially
		}
	}
	if missing > 0 || partial > 0 || mismatch > 0 {
		t.Fatalf("acked bulks: %d fully missing, %d PARTIAL (atomicity broken), %d mismatched, of %d",
			missing, partial, mismatch, len(allBulks))
	}
	t.Logf("PASS: all %d acknowledged bulks (%d objects) survived %d hard kills, none partial or corrupted",
		len(allBulks), len(allBulks)*batchSize, cycles)
}
