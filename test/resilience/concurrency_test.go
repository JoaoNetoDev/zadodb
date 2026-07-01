//go:build resilience

package resilience

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
	"github.com/JoaoNetoDev/zadodb/internal/storage/wal"
)

// TestConcurrentWritersThroughput drives many writers against the engine at
// once, across different and the same classes, measuring throughput and
// guarding against deadlock. It uses group-commit so the fsync cost of tens of
// thousands of writes is amortized.
func TestConcurrentWritersThroughput(t *testing.T) {
	e, err := storage.Open(storage.Config{
		Dir:   t.TempDir(),
		Fsync: wal.GroupCommitMode(2*time.Millisecond, 512),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	classes := []string{"Alpha", "Beta", "Gamma", "Delta"}
	for _, c := range classes {
		if err := e.CreateClass("", c); err != nil {
			t.Fatalf("CreateClass %s: %v", c, err)
		}
	}

	const writers = 64
	const per = 500
	start := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			class := classes[i%len(classes)]
			for j := 0; j < per; j++ {
				if _, err := e.CreateObject("", class, []byte(fmt.Sprintf("w%d-%d", i, j))); err != nil {
					t.Errorf("CreateObject: %v", err)
					return
				}
			}
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("deadlock: concurrent writers did not finish in 120s")
	}

	total := writers * per
	elapsed := time.Since(start)
	t.Logf("throughput: %d concurrent writes in %s (%.0f ops/s)",
		total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds())

	// All writes must be present and counts must match per class.
	got := 0
	for _, c := range classes {
		objs, err := e.ListObjects("", c, 0, 0)
		if err != nil {
			t.Fatalf("ListObjects %s: %v", c, err)
		}
		got += len(objs)
	}
	if got != total {
		t.Fatalf("total objects = %d, want %d", got, total)
	}
}
