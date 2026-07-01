// Package loadgen drives concurrent write load against a running ZadoDB HTTP
// server and records exactly which writes were acknowledged (HTTP 201).
//
// The resilience contract hinges on this: the server only returns 201 after the
// write is fsynced, so every recorded Ack MUST survive a subsequent hard kill.
// Writes that never got an ack may or may not survive — that is allowed.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Ack is a write the server confirmed durable: object id and its unique marker.
type Ack struct {
	ID int64
	N  int64
}

// Result accumulates acknowledged writes.
type Result struct {
	mu   sync.Mutex
	acks []Ack
	wg   sync.WaitGroup
}

// Acks returns a copy of the acknowledged writes recorded so far.
func (r *Result) Acks() []Ack {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Ack(nil), r.acks...)
}

// Wait blocks until all workers have stopped (e.g. after the server dies).
func (r *Result) Wait() { r.wg.Wait() }

func (r *Result) record(a Ack) {
	r.mu.Lock()
	r.acks = append(r.acks, a)
	r.mu.Unlock()
}

// Run launches workers that POST objects to baseURL/v1/classes/class/objects
// until ctx is cancelled or the server stops accepting connections. Each object
// carries a unique {"n": N} marker used later to verify integrity.
func Run(ctx context.Context, baseURL, class string, workers int) *Result {
	res := &Result{}
	var counter atomic.Int64
	url := fmt.Sprintf("%s/v1/classes/%s/objects", baseURL, class)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < workers; i++ {
		res.wg.Add(1)
		go func() {
			defer res.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				n := counter.Add(1)
				body, _ := json.Marshal(map[string]any{"n": n})
				req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
				resp, err := client.Do(req)
				if err != nil {
					return // server likely gone (killed): stop cleanly
				}
				if resp.StatusCode == http.StatusCreated {
					var m struct {
						ID int64 `json:"id"`
					}
					json.NewDecoder(resp.Body).Decode(&m)
					resp.Body.Close()
					res.record(Ack{ID: m.ID, N: n})
				} else {
					resp.Body.Close()
				}
			}
		}()
	}
	return res
}
