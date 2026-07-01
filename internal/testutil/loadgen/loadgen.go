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

// BulkAck is one acknowledged bulk: the ids returned and the markers sent, in
// order (ids[i] corresponds to marker ns[i]).
type BulkAck struct {
	IDs []int64
	Ns  []int64
}

// BulkResult accumulates acknowledged bulk inserts.
type BulkResult struct {
	mu    sync.Mutex
	bulks []BulkAck
	wg    sync.WaitGroup
}

// Bulks returns a copy of the acknowledged bulks recorded so far.
func (r *BulkResult) Bulks() []BulkAck {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BulkAck(nil), r.bulks...)
}

// Wait blocks until all workers have stopped.
func (r *BulkResult) Wait() { r.wg.Wait() }

// RunBulk launches workers that POST batches of `batchSize` objects to the bulk
// endpoint until ctx is cancelled or the server stops. Each acknowledged bulk
// (HTTP 201) is recorded whole; its objects MUST all survive a later crash.
func RunBulk(ctx context.Context, baseURL, class string, workers, batchSize int) *BulkResult {
	res := &BulkResult{}
	var counter atomic.Int64
	url := fmt.Sprintf("%s/v1/classes/%s/objects/bulk", baseURL, class)
	client := &http.Client{Timeout: 10 * time.Second}

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
				ns := make([]int64, batchSize)
				items := make([]map[string]any, batchSize)
				for j := 0; j < batchSize; j++ {
					n := counter.Add(1)
					ns[j] = n
					items[j] = map[string]any{"n": n}
				}
				body, _ := json.Marshal(items)
				req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				if resp.StatusCode == http.StatusCreated {
					var m struct {
						IDs []int64 `json:"ids"`
					}
					json.NewDecoder(resp.Body).Decode(&m)
					resp.Body.Close()
					if len(m.IDs) == batchSize {
						res.mu.Lock()
						res.bulks = append(res.bulks, BulkAck{IDs: m.IDs, Ns: ns})
						res.mu.Unlock()
					}
				} else {
					resp.Body.Close()
				}
			}
		}()
	}
	return res
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
