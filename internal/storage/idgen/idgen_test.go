package idgen

import (
	"sync"
	"testing"
)

func TestNextStartsAtOne(t *testing.T) {
	g := New()
	if id := g.Next("A"); id != 1 {
		t.Fatalf("first Next = %d, want 1", id)
	}
	if id := g.Next("A"); id != 2 {
		t.Fatalf("second Next = %d, want 2", id)
	}
	// Independent counters per class.
	if id := g.Next("B"); id != 1 {
		t.Fatalf("class B first Next = %d, want 1", id)
	}
}

func TestObserveResumesPastMax(t *testing.T) {
	g := New()
	g.Observe("A", 5)
	g.Observe("A", 3) // lower id must not lower the counter
	if got := g.Current("A"); got != 5 {
		t.Fatalf("Current = %d, want 5", got)
	}
	if id := g.Next("A"); id != 6 {
		t.Fatalf("Next after Observe = %d, want 6", id)
	}
}

func TestDrop(t *testing.T) {
	g := New()
	g.Next("A")
	g.Next("A")
	g.Drop("A")
	if id := g.Next("A"); id != 1 {
		t.Fatalf("Next after Drop = %d, want 1", id)
	}
}

func TestConcurrentNextNoDuplicates(t *testing.T) {
	g := New()
	const goroutines = 50
	const per = 200
	var mu sync.Mutex
	seen := make(map[int64]bool, goroutines*per)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				id := g.Next("A")
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate id %d", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*per {
		t.Fatalf("got %d unique ids, want %d", len(seen), goroutines*per)
	}
	if got := g.Current("A"); got != goroutines*per {
		t.Fatalf("Current = %d, want %d", got, goroutines*per)
	}
}
