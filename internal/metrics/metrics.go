// Package metrics exposes a minimal Prometheus-format /metrics endpoint and
// an HTTP middleware that records request latency, without pulling in a
// third-party client library — the whole app has one dependency
// (modernc.org/sqlite) and this keeps it that way.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// buckets are upper bounds in seconds, matching Prometheus's own default
// client histogram buckets so dashboards built against other apps' metrics
// still make sense against this one.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type key struct {
	method string
	route  string
	status int
}

// histogram counts, per bucket, requests whose latency fell at or below
// that bucket's upper bound. counts[len(buckets)] is the +Inf overflow
// bucket for anything slower than the largest named bucket.
type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

var (
	mu       sync.Mutex
	hists    = map[key]*histogram{}
	inFlight int64
)

// streamingRoutes holds the route patterns whose responses are long-lived
// SSE streams rather than ordinary request/response cycles. Instrument
// checks this to keep those connections out of the latency histogram and
// in-flight gauge — see the comment where it's consulted for why.
var streamingRoutes = map[string]bool{
	"GET /live":                          true,
	"GET /collections/{collection}/live": true,
}

// live* track the SSE hub's own lifecycle, separately from the request
// metrics above: a stream is one long-lived "request" by HTTP's accounting
// but is really a connection with its own open/close and per-event
// counters, so it gets gauge/counter semantics instead of a latency
// histogram. Plain int64s with atomic ops, matching inFlight above, rather
// than the sync/atomic typed wrappers — no reason to mix styles in a file
// this small.
var (
	liveConnsActive   int64
	liveEventsSent    int64
	liveEventsDropped int64
)

// LiveConnOpened records that an SSE stream started serving. Call sites
// live in internal/live and internal/web, which is why this is exported —
// this package must stay dependency-free, so it can't import theirs to wire
// the callback the other way around.
func LiveConnOpened() {
	atomic.AddInt64(&liveConnsActive, 1)
}

// LiveConnClosed records that an SSE stream stopped serving, for whatever
// reason (client disconnect, slow-consumer drop, server shutdown).
func LiveConnClosed() {
	atomic.AddInt64(&liveConnsActive, -1)
}

// LiveEventSent records one event successfully queued to a subscriber's
// channel.
func LiveEventSent() {
	atomic.AddInt64(&liveEventsSent, 1)
}

// LiveEventDropped records one event that couldn't be delivered because the
// subscriber's buffered channel was full. The hub sends without blocking, so
// a reader that has stalled costs it an event rather than costing every
// publisher — which in this app means an HTTP handler — a stall of its own.
// That trade is only defensible while it stays visible, and this counter is
// what makes it visible; a persistently rising value means subscribers are
// being dropped and resyncing rather than streaming.
func LiveEventDropped() {
	atomic.AddInt64(&liveEventsDropped, 1)
}

// patternHandler is the one method of *http.ServeMux that Instrument needs:
// the matched route pattern (e.g. "GET /entries/{id}"), not the raw path
// (e.g. "GET /entries/42"). Labeling by raw path would give the histogram
// unbounded cardinality — one series per row ever created, not per
// endpoint — so this is what makes per-route latency possible at all.
type patternHandler interface {
	Handler(r *http.Request) (http.Handler, string)
}

// Instrument wraps h, recording a request-duration histogram labeled by
// method, route and status code, plus a gauge of requests currently in
// flight.
//
// The route label is taken from the first of h itself (if it's a
// *http.ServeMux, checked via patternHandler) and extraRouters that
// returns a non-empty pattern for the request, later entries overriding
// earlier ones. A single flat mux needs nothing extra: h supplies its own
// patterns directly. Passing more is only for a layered setup like list's,
// where an outer mux (unauthenticated routes plus a "/" catch-all) wraps
// an inner one (the real per-endpoint patterns, behind auth middleware the
// outer mux can't see through) — there, h is the outer mux and the inner
// one is passed as an extra router so its more specific pattern wins.
// Anything genuinely unmatched — a 404, a route no router recognises — is
// labeled "unmatched" rather than the raw path, for the same cardinality
// reason.
func Instrument(h http.Handler, extraRouters ...patternHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := ""
		if router, ok := h.(patternHandler); ok {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		for _, router := range extraRouters {
			if _, pattern := router.Handler(r); pattern != "" {
				route = pattern
			}
		}
		if route == "" {
			route = "unmatched"
		}

		// The route has to be known before the in-flight gauge is touched,
		// not after, because streaming routes must never increment it: an
		// SSE connection can stay open for hours, and a gauge that only goes
		// up for the duration of a stream is a gauge that lies about load
		// for hours. Same reasoning excludes them from record() below —
		// live_connections_active (see LiveConnOpened/Closed) is the correct
		// gauge for those connections, not this one.
		streaming := streamingRoutes[route]
		if !streaming {
			atomic.AddInt64(&inFlight, 1)
			defer atomic.AddInt64(&inFlight, -1)
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		if !streaming {
			record(r.Method, route, sw.status, time.Since(start).Seconds())
		}
	})
}

// statusWriter captures the status code a handler wrote, defaulting to 200
// since http.ResponseWriter.Write implicitly sends that status if the
// handler never calls WriteHeader itself.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap gives back the wrapped ResponseWriter so http.NewResponseController
// can see through this wrapper to whatever the underlying writer actually
// supports. Without it, embedding the http.ResponseWriter interface (rather
// than a concrete type) hides Flush/Hijack/deadline-setting from
// ResponseController entirely — it has no interface to type-assert and no
// Unwrap chain to walk, so it returns http.ErrNotSupported even though the
// real connection supports flushing fine. That's silent: nothing panics or
// logs, a streaming handler just never gets its bytes out until the buffer
// fills or the connection ends, which is exactly what SSE cannot tolerate.
// otelhttp, which wraps outside this middleware, doesn't have this problem
// because it uses httpsnoop.Wrap, which preserves the concrete writer's
// optional interfaces by construction instead of relying on Unwrap.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func record(method, route string, status int, seconds float64) {
	mu.Lock()
	defer mu.Unlock()
	k := key{method: method, route: route, status: status}
	h, ok := hists[k]
	if !ok {
		h = &histogram{counts: make([]uint64, len(buckets)+1)}
		hists[k] = h
	}
	h.count++
	h.sum += seconds
	// SearchFloat64s returns the first bucket whose bound is >= seconds —
	// exactly the one bucket (of the fixed set) this observation belongs
	// to before cumulative sums are computed at render time. A value past
	// every named bucket lands on len(buckets), the +Inf slot.
	h.counts[sort.SearchFloat64s(buckets, seconds)]++
}

// Handler renders every recorded histogram, plus the in-flight gauge, in
// Prometheus text exposition format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request latency in seconds.")
		fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")

		keys := make([]key, 0, len(hists))
		for k := range hists {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].route != keys[j].route {
				return keys[i].route < keys[j].route
			}
			if keys[i].method != keys[j].method {
				return keys[i].method < keys[j].method
			}
			return keys[i].status < keys[j].status
		})

		for _, k := range keys {
			h := hists[k]
			labels := fmt.Sprintf("method=%q,route=%q,status=%q", k.method, k.route, strconv.Itoa(k.status))
			var cumulative uint64
			for i, b := range buckets {
				cumulative += h.counts[i]
				fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(b, 'g', -1, 64), cumulative)
			}
			cumulative += h.counts[len(buckets)]
			fmt.Fprintf(w, "http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, cumulative)
			fmt.Fprintf(w, "http_request_duration_seconds_sum{%s} %s\n", labels, strconv.FormatFloat(h.sum, 'g', -1, 64))
			fmt.Fprintf(w, "http_request_duration_seconds_count{%s} %d\n", labels, h.count)
		}

		fmt.Fprintln(w, "# HELP http_requests_in_flight HTTP requests currently being served.")
		fmt.Fprintln(w, "# TYPE http_requests_in_flight gauge")
		fmt.Fprintf(w, "http_requests_in_flight %d\n", atomic.LoadInt64(&inFlight))

		fmt.Fprintln(w, "# HELP live_connections_active Open SSE live-update streams.")
		fmt.Fprintln(w, "# TYPE live_connections_active gauge")
		fmt.Fprintf(w, "live_connections_active %d\n", atomic.LoadInt64(&liveConnsActive))

		fmt.Fprintln(w, "# HELP live_events_sent_total Live-update events written to a subscriber.")
		fmt.Fprintln(w, "# TYPE live_events_sent_total counter")
		fmt.Fprintf(w, "live_events_sent_total %d\n", atomic.LoadInt64(&liveEventsSent))

		fmt.Fprintln(w, "# HELP live_events_dropped_total Live-update events dropped because a subscriber's buffer was full.")
		fmt.Fprintln(w, "# TYPE live_events_dropped_total counter")
		fmt.Fprintf(w, "live_events_dropped_total %d\n", atomic.LoadInt64(&liveEventsDropped))
	})
}
