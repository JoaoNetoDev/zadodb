package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	eng, err := storage.Open(storage.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("engine open: %v", err)
	}
	srv := New(eng, "127.0.0.1:0", log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		eng.Close()
	})
	return ts
}

func do(t *testing.T, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, buf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	var m map[string]any
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(data) > 0 {
		json.Unmarshal(data, &m)
	}
	return resp, m
}

func TestHTTPCRUDFlow(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL

	// Health.
	resp, m := do(t, "GET", base+"/v1/health", nil)
	if resp.StatusCode != 200 || m["status"] != "ok" {
		t.Fatalf("health = %d %v", resp.StatusCode, m)
	}

	// Create class.
	resp, _ = do(t, "POST", base+"/v1/classes", map[string]any{"name": "Pessoa"})
	if resp.StatusCode != 201 {
		t.Fatalf("create class = %d", resp.StatusCode)
	}
	// Duplicate class -> 409.
	resp, _ = do(t, "POST", base+"/v1/classes", map[string]any{"name": "Pessoa"})
	if resp.StatusCode != 409 {
		t.Fatalf("dup class = %d, want 409", resp.StatusCode)
	}

	// Create object.
	resp, m = do(t, "POST", base+"/v1/classes/Pessoa/objects", map[string]any{"nome": "João", "idade": 30})
	if resp.StatusCode != 201 {
		t.Fatalf("create object = %d", resp.StatusCode)
	}
	if m["id"].(float64) != 1 || m["nome"] != "João" {
		t.Fatalf("created object = %v", m)
	}

	// Get object.
	resp, m = do(t, "GET", base+"/v1/classes/Pessoa/objects/1", nil)
	if resp.StatusCode != 200 || m["nome"] != "João" {
		t.Fatalf("get object = %d %v", resp.StatusCode, m)
	}

	// Update object.
	resp, m = do(t, "PUT", base+"/v1/classes/Pessoa/objects/1", map[string]any{"nome": "João Neto", "idade": 31})
	if resp.StatusCode != 200 || m["nome"] != "João Neto" {
		t.Fatalf("put object = %d %v", resp.StatusCode, m)
	}

	// List objects.
	do(t, "POST", base+"/v1/classes/Pessoa/objects", map[string]any{"nome": "Maria"})
	resp, m = do(t, "GET", base+"/v1/classes/Pessoa/objects", nil)
	if resp.StatusCode != 200 || m["count"].(float64) != 2 {
		t.Fatalf("list = %d %v", resp.StatusCode, m)
	}

	// Delete object.
	resp, _ = do(t, "DELETE", base+"/v1/classes/Pessoa/objects/1", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	resp, _ = do(t, "GET", base+"/v1/classes/Pessoa/objects/1", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("get deleted = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPBulkInsert(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL

	do(t, "POST", base+"/v1/classes", map[string]any{"name": "Item"})

	// Bulk create via a JSON array.
	items := make([]map[string]any, 300)
	for i := range items {
		items[i] = map[string]any{"n": i, "label": fmt.Sprintf("item-%d", i)}
	}
	b, _ := json.Marshal(items)
	resp, err := http.Post(base+"/v1/classes/Item/objects/bulk", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("bulk post: %v", err)
	}
	var out struct {
		IDs   []int64 `json:"ids"`
		Count int     `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 201 || out.Count != 300 || len(out.IDs) != 300 {
		t.Fatalf("bulk = %d count=%d ids=%d", resp.StatusCode, out.Count, len(out.IDs))
	}

	// Verify a couple made it with correct payload.
	_, m := do(t, "GET", fmt.Sprintf("%s/v1/classes/Item/objects/%d", base, out.IDs[100]), nil)
	if int(m["n"].(float64)) != 100 || m["label"] != "item-100" {
		t.Fatalf("object 101 = %v", m)
	}

	// List reflects all 300.
	_, lm := do(t, "GET", base+"/v1/classes/Item/objects?limit=1000", nil)
	if int(lm["count"].(float64)) != 300 {
		t.Fatalf("list count = %v, want 300", lm["count"])
	}

	// Bulk on missing class -> 404; non-array body -> 400.
	resp2, _ := http.Post(base+"/v1/classes/Ghost/objects/bulk", "application/json", bytes.NewReader(b))
	if resp2.StatusCode != 404 {
		t.Fatalf("bulk missing class = %d, want 404", resp2.StatusCode)
	}
	resp2.Body.Close()
	resp3, _ := http.Post(base+"/v1/classes/Item/objects/bulk", "application/json", bytes.NewReader([]byte(`{"not":"array"}`)))
	if resp3.StatusCode != 400 {
		t.Fatalf("non-array bulk = %d, want 400", resp3.StatusCode)
	}
	resp3.Body.Close()
}

func TestHTTPErrorCases(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL

	// Object on missing class -> 404.
	resp, _ := do(t, "POST", base+"/v1/classes/Ghost/objects", map[string]any{"x": 1})
	if resp.StatusCode != 404 {
		t.Fatalf("object missing class = %d, want 404", resp.StatusCode)
	}
	// Bad object id -> 400.
	do(t, "POST", base+"/v1/classes", map[string]any{"name": "A"})
	resp, _ = do(t, "GET", base+"/v1/classes/A/objects/abc", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("bad id = %d, want 400", resp.StatusCode)
	}
	// Missing object -> 404.
	resp, _ = do(t, "GET", base+"/v1/classes/A/objects/999", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("missing object = %d, want 404", resp.StatusCode)
	}
	// Invalid class name -> 400.
	resp, _ = do(t, "POST", base+"/v1/classes", map[string]any{"name": "bad name!"})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid name = %d, want 400", resp.StatusCode)
	}
	// Empty body to create class -> 400.
	resp, _ = do(t, "POST", base+"/v1/classes", map[string]any{})
	if resp.StatusCode != 400 {
		t.Fatalf("empty name = %d, want 400", resp.StatusCode)
	}
}
