package web

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"list/internal/live"
	"list/internal/metrics"
)

// liveOrigin reads the per-tab id live.js stamps onto every htmx request, so
// a mutation's own publish carries the tab that caused it. A browser uses it
// to ignore the echo of its own change rather than re-fetching and possibly
// clobbering an edit in progress.
func liveOrigin(r *http.Request) string { return r.Header.Get("X-Live-Origin") }

// handleUserLive is the index page's stream: everything that can change a
// user's own collections list or membership, across every collection they
// belong to.
func (s *Server) handleUserLive(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	s.stream(w, r, u.ID, live.UserTopic(u.ID))
}

// handleCollectionLive is a collection page's stream. It subscribes to both
// the collection's own topic and the user's, so losing access (an "access"
// or "collections" event on the user topic) reaches an open collection page
// exactly the way it reaches the index.
func (s *Server) handleCollectionLive(w http.ResponseWriter, r *http.Request) {
	u, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	s.stream(w, r, u.ID, live.CollectionTopic(c.ID), live.UserTopic(u.ID))
}

// stream serves a Server-Sent Events response on the given topics until the
// client disconnects, the subscriber is evicted or dropped, or the hub is
// closed.
//
// Callers must finish all authorisation (collectionFor and friends) before
// calling stream — nothing here may run before the caller has decided the
// request is allowed, because once execution reaches Subscribe below there is
// no way left to send a normal 404/403 response; the only thing left to send
// is stream frames.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, userID int64, topics ...string) {
	// main.go sets ReadTimeout/WriteTimeout as absolute deadlines at request
	// start; left in place, a stream would be killed by WriteTimeout well
	// before an idle client would ever notice. Clearing them here, rather
	// than loosening the server config, keeps that protection for every
	// ordinary request and only lifts it for the one handler that needs to
	// stay open indefinitely.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		// This only fails if the Unwrap chain from statusWriter down to the
		// real connection is broken, in which case Flush would fail the same
		// way later and the stream would silently stall rather than error.
		// Fail loudly now instead.
		log.Printf("stream: SetWriteDeadline: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("stream: SetReadDeadline: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ch, cancel, ok := s.live.Subscribe(userID, topics...)
	if !ok {
		http.Error(w, "too many live connections", http.StatusTooManyRequests)
		return
	}
	defer cancel()

	metrics.LiveConnOpened()
	defer metrics.LiveConnClosed()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	// Cache-Control: no-store is already set by authenticate as the default
	// for every response, streams included; nothing about a stream calls for
	// this one to take the cacheImmutable override instead.
	w.Header().Set("X-Accel-Buffering", "no") // belt and braces against a buffering proxy

	// Subscribe happens before any byte is written, so this preamble is proof
	// to the client that it is subscribed — a test can read it as a sync
	// barrier, then issue a mutation, then deterministically expect a
	// following "data:" line, with no sleeps. Do not reorder Subscribe after
	// this write.
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	if err := rc.Flush(); err != nil {
		return
	}

	// 25s is comfortably under Cloudflare's ~100s idle-connection cutoff, so
	// the heartbeat keeps the stream alive through that edge as well as
	// through the cleared WriteTimeout above.
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()

	for {
		select {
		case msg, open := <-ch:
			if !open {
				return // dropped for a full buffer, evicted, or the hub closed
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
