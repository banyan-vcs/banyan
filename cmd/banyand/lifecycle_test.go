package main

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestServeHTTPDrainsInFlight pins the graceful-shutdown contract: after
// ctx is cancelled, an in-flight request (a mid-transfer pack stream) runs
// to completion before serveHTTP returns, ErrServerClosed is swallowed,
// and the listener is gone afterwards — a rolling restart never cuts a
// transfer, and the process can exit cleanly.
func TestServeHTTPDrainsInFlight(t *testing.T) {
	addr := "127.0.0.1:18123"
	ctx, cancel := context.WithCancel(context.Background())
	var finished atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond) // transfer still going when the signal lands
		finished.Store(true)
		_, _ = fmt.Fprint(w, "done")
	})
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, addr, mux) }()

	for i := 0; i < 100; i++ { // wait for the listener
		if _, err := http.Get("http://" + addr + "/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("server never came up: %v", err)
	}

	cancel() // the termination signal, with the request mid-flight
	if err := <-done; err != nil {
		t.Fatalf("serveHTTP returned %v (ErrServerClosed must be swallowed)", err)
	}
	if !finished.Load() {
		t.Fatal("in-flight request was cut — drain must let it finish")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Fatal("listener must be closed after drain")
	}
}
