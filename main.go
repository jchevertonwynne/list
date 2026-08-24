// Command list is a shared to-do list for the k3s cluster described in
// jchevertonwynne/homelab, served at list.jchevertonwynne.uk.
//
// It has no login of its own. Cloudflare Access authenticates every request
// at the edge and passes the address on in a header; see internal/web/auth.go
// for why trusting that header is reasonable here and what it would take for
// it to stop being reasonable.
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

	"list/internal/store"
	"list/internal/tracing"
	"list/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "/var/lib/list/list.db", "path to the SQLite database")
	devUser := flag.String("dev-user", "", "email to assume when no Access header is present; local development only")
	otelEndpoint := flag.String("otel-endpoint", "", "host:port of an OTLP/gRPC trace collector; tracing is disabled if empty")
	flag.Parse()

	shutdownTracing, err := tracing.Init(context.Background(), "list", *otelEndpoint)
	if err != nil {
		log.Fatalf("init tracing: %v", err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		// A database that will not open is a startup failure, not something
		// to serve 500s over: the pod should crashloop visibly rather than
		// look healthy while nothing works.
		log.Fatalf("opening %s: %v", *dbPath, err)
	}
	defer db.Close()

	if *devUser != "" {
		log.Printf("WARNING: -dev-user is set to %q; every unauthenticated request will be treated as that user", *devUser)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: web.New(db, *devUser).Handler(),
		// A service reachable from the internet needs these. Without
		// ReadHeaderTimeout a single client can hold a connection open
		// indefinitely by dribbling out headers.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds before
	// SIGKILL. SQLite is committed by the time a request returns, so there is
	// nothing to flush here beyond letting in-flight requests finish.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on %s, database at %s", *addr, *dbPath)
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
	if err := shutdownTracing(shutdownCtx); err != nil {
		log.Printf("tracing shutdown: %v", err)
	}
}
