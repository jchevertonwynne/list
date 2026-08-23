package assets

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// served lists every file the deployed page depends on. All four must be
// reachable from the same URL directory, or the font-relative-path trick in
// beer.min.css breaks and icons render as text.
var served = []string{
	"/static/beer.min.css",
	"/static/beer.min.js",
	"/static/htmx.min.js",
	"/static/material-symbols-outlined.woff2",
}

func TestServesEmbeddedFiles(t *testing.T) {
	h := Handler()

	for _, path := range served {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("body is empty")
			}

			// Baked into the image at build time: bytes cannot change
			// without a new image, which is a new pod, so the strongest
			// cache directive is always safe.
			want := "public, max-age=31536000, immutable"
			if got := rec.Header().Get("Cache-Control"); got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
		})
	}
}

// TestWOFF2ContentType guards against mime.TypeByExtension not knowing
// ".woff2" on some platforms, which would otherwise serve the font as
// application/octet-stream (or worse, sniffed as something browsers refuse to
// use as a font) and silently break icon rendering.
func TestWOFF2ContentType(t *testing.T) {
	h := Handler()

	req := httptest.NewRequest(http.MethodGet, "/static/material-symbols-outlined.woff2", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "font/woff2" {
		t.Errorf("Content-Type = %q, want %q", got, "font/woff2")
	}
}

// TestPathTraversalDoesNotEscape asserts that a request engineered to walk
// back out of the embedded "static" directory cannot reach anything outside
// it — there is no on-disk sibling to leak here, but the same handler shape
// gets copy-pasted into services that do have one.
func TestPathTraversalDoesNotEscape(t *testing.T) {
	h := Handler()

	req := httptest.NewRequest(http.MethodGet, "/static/../assets.go", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("traversal request returned 200 with body %q", rec.Body.String())
	}
}
