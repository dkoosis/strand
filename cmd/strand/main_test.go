package main

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestServeDrainsInFlight is the regression for strand-15n: serve must wait for
// an in-flight request to finish before returning, not drop it when the
// shutdown context is cancelled. A handler that sleeps past the cancel point
// must still complete and have its body recorded by the client.
func TestServeDrainsInFlight(t *testing.T) {
	// The handler blocks on releaseHandler so the test controls exactly when the
	// in-flight request finishes — no sleeps, and no race over when to cancel.
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var handlerDone atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(handlerStarted)
		<-releaseHandler
		handlerDone.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serve(ctx, httpSrv, ln) }()

	// Fire a slow request, then cancel mid-flight. The drain must let it land.
	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			reqDone <- err
			return
		}
		resp.Body.Close()
		reqDone <- nil
	}()

	<-handlerStarted // the request is now in flight
	cancel()         // simulate Ctrl-C while the request is in flight

	// The regression: a broken serve returns at the START of Shutdown, before
	// the in-flight request lands. While the handler is still blocked, serve
	// must NOT have returned — the drain handshake holds it.
	select {
	case err := <-served:
		t.Fatalf("serve returned mid-drain (err=%v) — in-flight request would be dropped", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseHandler) // let the in-flight request complete

	if err := <-reqDone; err != nil {
		t.Fatalf("in-flight request was dropped: %v", err)
	}
	if !handlerDone.Load() {
		t.Fatal("handler did not complete — request was not drained")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after drain — handshake leaked")
	}
}
