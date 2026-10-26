package debug

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The ops-plane endpoints must answer with parseable payloads: the pprof
// index lists the family, heap profiles arrive as gzip'd protobuf, the
// stats snapshot carries the memory-chain numbers, and Start with an empty
// address stays a no-op.
func TestOpsPlaneEndpoints(t *testing.T) {
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	get := func(path string) (int, []byte) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	code, body := get("/debug/pprof/")
	if code != 200 {
		t.Fatalf("pprof index: %d", code)
	}
	for _, want := range []string{"heap", "goroutine", "profile", "trace", "block", "mutex"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("pprof index missing %q:\n%s", want, body)
		}
	}

	code, body = get("/debug/pprof/heap")
	if code != 200 {
		t.Fatalf("heap profile: %d", code)
	}
	// The pprof family serves gzip'd protobuf; the magic is the cheap
	// end-to-end check that a profiler can consume it.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("heap profile: not gzip (first bytes % x)", body[:min(4, len(body))])
	}

	code, body = get("/debug/stats")
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	var st struct {
		GoVersion       string `json:"go_version"`
		GomemlimitBytes int64  `json:"gomemlimit_bytes"`
		Mem             struct {
			HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
			NumGC          uint64 `json:"num_gc"`
		} `json:"mem"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, body)
	}
	if st.GoVersion == "" {
		t.Fatal("stats: go_version empty")
	}

	code, _ = get("/debug/vars")
	if code != 200 {
		t.Fatalf("vars: %d", code)
	}

	Start(Options{}) // empty addr: must not panic, must not listen
}
