package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The probes depend on this, so it is worth a test even though it is three
// lines: a /healthz that starts returning 500 takes the pod down.
func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body = %q, want it to say ok", rec.Body.String())
	}
}

func TestRoot(t *testing.T) {
	rec := httptest.NewRecorder()
	handleRoot(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
