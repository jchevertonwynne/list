package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"list/internal/store"
)

// TestBackupSnapshotContainsWALResidentWrites is the test that earns its keep.
// The whole reason /backup.db exists rather than the CronJob copying list.db
// off the volume is that WAL mode leaves recent commits outside the main
// database file. Nothing here checkpoints between the writes and the request,
// so if VACUUM INTO were ever swapped for something that reads only list.db,
// this fails.
func TestBackupSnapshotContainsWALResidentWrites(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()

	u, err := db.UserByEmail(ctx, owner)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, u.ID, "camping")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := db.CreateItem(ctx, c.ID, u.ID, "tent pegs", "the long ones"); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	rec := getBackup(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Write the snapshot out and open it as a database. Opening it at all is
	// most of the assertion: a torn or truncated SQLite file fails here.
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(snapshot, rec.Body.Bytes(), 0o600); err != nil {
		t.Fatalf("writing snapshot: %v", err)
	}
	restored, err := store.Open(snapshot)
	if err != nil {
		t.Fatalf("opening snapshot: %v", err)
	}
	defer restored.Close()

	items, err := restored.Items(ctx, c.ID)
	if err != nil {
		t.Fatalf("Items from snapshot: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("snapshot has %d items, want 1 — the WAL was not captured", len(items))
	}
	if items[0].Title != "tent pegs" {
		t.Errorf("snapshot item title = %q, want %q", items[0].Title, "tent pegs")
	}
}

// TestBackupNeedsNoAccessHeader pins the property the CronJob depends on: the
// in-cluster caller has no Cloudflare Access session, so a request with no
// identity header at all must still succeed. Every other route answers 403.
func TestBackupNeedsNoAccessHeader(t *testing.T) {
	h, _ := newTestServer(t)

	rec := getBackup(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no Access header", rec.Code)
	}

	// The contrast is the point: the same request shape against an app route
	// is refused.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	app := httptest.NewRecorder()
	h.ServeHTTP(app, r)
	if app.Code != http.StatusForbidden {
		t.Errorf("GET / with no header = %d, want 403", app.Code)
	}
}

// TestBackupHeaders covers the two headers a caller relies on: Content-Length,
// so a truncated transfer is detectable rather than arriving as a short but
// valid-looking file, and no-store, so the snapshot is never served from
// Cloudflare's edge cache.
func TestBackupHeaders(t *testing.T) {
	h, _ := newTestServer(t)
	rec := getBackup(t, h)

	if got, want := rec.Header().Get("Content-Type"), "application/vnd.sqlite3"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	length, err := strconv.Atoi(rec.Header().Get("Content-Length"))
	if err != nil {
		t.Fatalf("Content-Length = %q: %v", rec.Header().Get("Content-Length"), err)
	}
	if length != rec.Body.Len() {
		t.Errorf("Content-Length = %d, body is %d bytes", length, rec.Body.Len())
	}
}

// TestBackupIsRepeatable guards the specific way this handler could rot.
// VACUUM INTO refuses to write to a path that already exists, so a handler
// that reused a fixed temp path — or failed to clean up after itself — would
// pass once and 500 on every call after that. The CronJob calls it hourly
// forever, so "works the first time" is not the property wanted.
func TestBackupIsRepeatable(t *testing.T) {
	h, _ := newTestServer(t)
	for i := range 3 {
		if rec := getBackup(t, h); rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i+1, rec.Code)
		}
	}
}

// TestBackupFailureIs500 checks the error path reports rather than serving a
// zero-length body with a 200, which would look to the CronJob like a
// successful backup of an empty database — the worst available outcome.
func TestBackupFailureIs500(t *testing.T) {
	fs := &fakeStore{err: &testError{"vacuum exploded"}}
	h := New(fs, "").Handler()

	rec := getBackup(t, h)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("Content-Length set on a failed backup: %q", rec.Header().Get("Content-Length"))
	}
}

func getBackup(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/backup.db", nil))
	return rec
}
