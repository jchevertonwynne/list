// Command app is a starting point for a service on the k3s cluster described
// in jchevertonwynne/homelab. Replace this comment and handleRoot with
// whatever the app actually does; the rest is the shape the cluster expects.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /{$}", handleRoot)

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// A service reachable from the internet needs these. Without
		// ReadHeaderTimeout a single client can hold a connection open
		// indefinitely by dribbling out headers.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds before
	// SIGKILL. Anything that must be flushed on the way out goes after
	// Shutdown returns.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// handleHealthz is what the readiness and liveness probes hit. Keep it cheap
// and keep it free of dependencies: it runs several times a minute forever, so
// anything expensive here is expensive permanently, and a failing database
// should surface as a 500 on a real request rather than as a restart loop.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("hello\n"))
}
