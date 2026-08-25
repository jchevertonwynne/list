package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstrumentRecordsStatusAndCount(t *testing.T) {
	// hists is package-level state; give this test its own key so it can't
	// collide with counts left behind by other tests in the package.
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest("PROPFIND", "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}

	body := renderMetrics(t)
	// A plain http.HandlerFunc (not a *http.ServeMux) can't offer a route
	// pattern, so this falls back to "unmatched" rather than the raw path.
	wantLine := `http_request_duration_seconds_count{method="PROPFIND",route="unmatched",status="418"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q; got:\n%s", wantLine, body)
	}
}

func TestInstrumentDefaultsStatusTo200(t *testing.T) {
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler never calls WriteHeader — net/http implicitly sends 200
		// on the first Write, and statusWriter must default the same way.
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("TRACE", "/whatever", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := renderMetrics(t)
	wantLine := `http_request_duration_seconds_count{method="TRACE",route="unmatched",status="200"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q; got:\n%s", wantLine, body)
	}
}

func TestInstrumentLabelsByMuxPattern(t *testing.T) {
	// A *http.ServeMux passed directly satisfies patternHandler itself, so
	// no extraRouters are needed to get the matched pattern rather than the
	// raw path.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Instrument(mux)

	req := httptest.NewRequest("GET", "/widgets/42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := renderMetrics(t)
	wantLine := `http_request_duration_seconds_count{method="GET",route="GET /widgets/{id}",status="200"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q (raw path would blow up cardinality); got:\n%s", wantLine, body)
	}
}

func TestInstrumentFallsBackToExtraRouterPattern(t *testing.T) {
	// Mirrors list's layered setup: an outer mux only knows a "/" catch-all
	// for authenticated routes, and the real per-endpoint pattern lives on
	// an inner mux the outer one wraps behind other middleware. The inner
	// mux, passed as an extraRouter, should win over the outer's "/".
	inner := http.NewServeMux()
	inner.HandleFunc("GET /collections/{collection}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	outer := http.NewServeMux()
	outer.Handle("/", inner)

	handler := Instrument(outer, inner)

	req := httptest.NewRequest("GET", "/collections/groceries", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := renderMetrics(t)
	wantLine := `http_request_duration_seconds_count{method="GET",route="GET /collections/{collection}",status="200"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q; got:\n%s", wantLine, body)
	}
}

func TestHandlerExposesInFlightGauge(t *testing.T) {
	body := renderMetrics(t)
	if !strings.Contains(body, "# TYPE http_requests_in_flight gauge") {
		t.Fatalf("metrics output missing in-flight gauge type line; got:\n%s", body)
	}
	// No request is in flight at the moment /metrics itself renders, since
	// Handler() only counts requests Instrument wraps, and nothing here is
	// blocked mid-request.
	if !strings.Contains(body, "http_requests_in_flight 0") {
		t.Fatalf("metrics output missing zeroed in-flight gauge; got:\n%s", body)
	}
}

func TestInstrumentPreservesFlushThroughUnwrap(t *testing.T) {
	// Regression test for statusWriter.Unwrap. Instrument's wrapper embeds
	// the http.ResponseWriter interface, which hides Flush from a type
	// assertion; without Unwrap, http.NewResponseController can't walk back
	// to the underlying writer and Flush silently returns ErrNotSupported —
	// silently is the problem, since nothing else here would ever catch it.
	var flushErr error
	handler := Instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/whatever", nil))

	if flushErr != nil {
		t.Fatalf("Flush() through Instrument = %v, want nil", flushErr)
	}
}

func TestInstrumentExcludesStreamingRoutesFromHistogram(t *testing.T) {
	// A streaming route (e.g. an SSE handler) can stay open for hours, so it
	// must never land in the latency histogram — one +Inf observation per
	// connection would make the histogram meaningless. A normal route on the
	// same mux must still be recorded as usual.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /gizmos/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Instrument(mux)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/live", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/gizmos/7", nil))

	body := renderMetrics(t)
	if strings.Contains(body, `route="GET /live"`) {
		t.Fatalf("metrics output should have no series for the streaming route; got:\n%s", body)
	}
	wantLine := `http_request_duration_seconds_count{method="GET",route="GET /gizmos/{id}",status="200"} 1`
	if !strings.Contains(body, wantLine) {
		t.Fatalf("metrics output missing %q for the non-streaming route; got:\n%s", wantLine, body)
	}
}

func TestLiveMetricsAppearAfterHooksCalled(t *testing.T) {
	// The four exported hooks are the only surface internal/live and
	// internal/web get, since this package can't depend on theirs. Exercise
	// each one and check both the values and that Handler() renders them as
	// proper gauge/counter series, matching the existing metrics' style.
	LiveConnOpened()
	LiveConnOpened()
	LiveConnClosed()
	LiveEventSent()
	LiveEventSent()
	LiveEventSent()
	LiveEventDropped()

	body := renderMetrics(t)
	for _, want := range []string{
		"# TYPE live_connections_active gauge",
		"live_connections_active 1",
		"# TYPE live_events_sent_total counter",
		"live_events_sent_total 3",
		"# TYPE live_events_dropped_total counter",
		"live_events_dropped_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q; got:\n%s", want, body)
		}
	}
}

func renderMetrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
