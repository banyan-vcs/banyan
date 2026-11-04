// lifecycle.go — process lifecycle for the banyand roles (production
// posture): one signal context for everything, and an HTTP serve wrapper
// that DRAINS instead of dying — a rolling restart must not cut an
// in-flight pack stream mid-transfer.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// drainGrace bounds how long in-flight requests may finish after
// SIGINT/SIGTERM: new connections are refused at once, active ones keep
// streaming, and whatever outlives the grace is cut.
const drainGrace = 15 * time.Second

// signalContext yields a context cancelled on the termination signals
// (Ctrl+C, container stop, systemd reload).
func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// serveHTTP runs mux on addr until ctx is done, then drains in-flight
// requests within drainGrace and returns. ErrServerClosed — the normal
// shutdown exit — is swallowed; every other serve error is returned.
func serveHTTP(ctx context.Context, addr string, mux *http.ServeMux) error {
	srv := &http.Server{Addr: addr, Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http %s: %v", addr, err)
		}
	}()
	<-ctx.Done()
	log.Printf("http %s: draining (grace %s)", addr, drainGrace)
	sctx, cancel := context.WithTimeout(context.Background(), drainGrace)
	defer cancel()
	return srv.Shutdown(sctx)
}
