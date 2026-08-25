package web

// live_test.go tests the SSE endpoints in live.go, which routes_test.go's
// existing pattern cannot reach: httptest.NewRecorder captures a response
// into a buffer after the handler returns, but a stream handler never
// returns while the connection is open, and http.NewResponseController has
// nothing real to control on a ResponseRecorder — Flush and the deadline
// setters fail immediately, since there is no underlying net.Conn. These
// tests instead spin up a real httptest.NewServer, so live.go's handler runs
// against a genuine connection the way it will in production, and a client
// can read frames off it as they arrive.
//
// Everything else about house style holds here: a real temp-file SQLite
// database (see routes_test.go's header and store_test.go's `open`), the
// owner/member/stranger fixture and do() from routes_test.go, and plain
// `if got != want { t.Fatalf(...) }` — no testify.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"list/internal/live"
	"list/internal/store"
)

// streamTimeout bounds every read in this file. Without it, a regression
// that made a stream never deliver an expected event would hang the test
// process instead of failing it; this turns that into an ordinary failure
// after a fixed wait rather than a stuck `go test`.
const streamTimeout = 10 * time.Second

// openStream connects to path as email and returns a reader positioned after
// the "retry: 3000" preamble, plus a func that closes the connection.
//
// That preamble is the whole trick that keeps these tests deterministic.
// stream() in live.go calls s.live.Subscribe *before* it writes a single
// byte, so by the time a client has read the preamble, the subscriber is
// guaranteed to be registered with the hub. A test can therefore read the
// preamble here, issue a mutation on a completely separate connection, and
// then expect a "data:" line next — no sleeps, no polling, and no window in
// which the mutation could race the subscription. Do not "simplify" this by
// skipping the preamble read; that removes the only thing making these tests
// reliable rather than flaky.
func openStream(t *testing.T, srv *httptest.Server, path, email string) (*bufio.Reader, func()) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Cf-Access-Authenticated-User-Email", email)

	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("opening stream %s as %s: %v", path, email, err)
	}
	closeFn := func() {
		resp.Body.Close()
		cancel()
	}

	if resp.StatusCode != http.StatusOK {
		closeFn()
		t.Fatalf("opening stream %s as %s: status = %d, want 200", path, email, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		closeFn()
		t.Fatalf("stream Content-Type = %q, want text/event-stream", ct)
	}

	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil {
		closeFn()
		t.Fatalf("reading preamble: %v", err)
	}
	if line != "retry: 3000\n" {
		closeFn()
		t.Fatalf("preamble = %q, want %q", line, "retry: 3000\n")
	}
	if _, err := r.ReadString('\n'); err != nil { // the blank line ending the SSE frame
		closeFn()
		t.Fatalf("reading preamble terminator: %v", err)
	}

	return r, closeFn
}

// readEvent reads the next "data: {...}" SSE frame from r, skipping blank
// lines and any ": ping" heartbeat comments, and decodes its JSON payload.
// The error is returned rather than fatalled so a caller that cares how the
// stream ended — see TestRemoveMemberRevokesAndCloses, which must tell a
// clean io.EOF apart from anything else — can inspect it.
func readEvent(r *bufio.Reader) (live.Event, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return live.Event{}, err
		}
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			continue
		}
		data, ok := strings.CutPrefix(trimmed, "data: ")
		if !ok {
			return live.Event{}, fmt.Errorf("unexpected SSE line: %q", trimmed)
		}
		if _, err := r.ReadString('\n'); err != nil { // the blank line ending this frame
			return live.Event{}, err
		}
		var ev live.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return live.Event{}, fmt.Errorf("unmarshalling event %q: %w", data, err)
		}
		return ev, nil
	}
}

// mustReadEvent is readEvent for tests that only care that an event arrived,
// not how a failure to arrive would look.
func mustReadEvent(t *testing.T, r *bufio.Reader) live.Event {
	t.Helper()
	ev, err := readEvent(r)
	if err != nil {
		t.Fatalf("reading event: %v", err)
	}
	return ev
}

// mustReadEventOfKind reads events until one has the given Kind, skipping
// anything else. It exists because a collection stream subscribes to both
// collection:{c} and user:{id} (handleCollectionLive), and notifyIndexes
// (routes.go) publishes a "collections" event to every member of a
// collection whenever any stream has a "user:" subscription open at all —
// which every test's own stream in this file is. So a test's own connection
// routinely receives its own "collections" echo interleaved with whatever
// "item" or "members" event it actually opened the stream to observe, and a
// fixed-position read is not reliable; filtering by kind is.
func mustReadEventOfKind(t *testing.T, r *bufio.Reader, kind string) live.Event {
	t.Helper()
	for {
		ev := mustReadEvent(t, r)
		if ev.Kind == kind {
			return ev
		}
	}
}

// doOrigin is do() (routes_test.go) plus an X-Live-Origin header, for the one
// test that needs to control it. do() itself is not touched — other tests
// depend on its exact signature — so this lives here instead.
func doOrigin(t *testing.T, h http.Handler, method, path, email, origin string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Cf-Access-Authenticated-User-Email", email)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Live-Origin", origin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// doMultipartOrigin is doMultipart (routes_test.go) plus an X-Live-Origin
// header, for the cover-upload counterpart to doOrigin above. doMultipart
// itself is not touched, for the same reason do() isn't: other tests depend
// on its exact signature.
func doMultipartOrigin(t *testing.T, h http.Handler, method, path, email, origin string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("cover", "cover.jpg")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write cover part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	r := httptest.NewRequest(method, path, &body)
	r.Header.Set("Cf-Access-Authenticated-User-Email", email)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("HX-Request", "true")
	r.Header.Set("X-Live-Origin", origin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A stranger must never see any part of a stream response, not even one that
// then errors, because that would mean stream() had already started writing
// headers before authorisation ran. Asserting both the status and that
// Content-Type never became text/event-stream is what proves collectionFor
// runs, and rejects, before any stream byte goes out — matching every other
// route's "404, not 403, and nothing else" contract (see authz.go).
func TestCollectionLive_MemberOK_StrangerNotFound(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// openStream itself asserts 200 and Content-Type: text/event-stream, so
	// simply succeeding here is the "member can open it" half of this test.
	_, closeStream := openStream(t, srv, path(c, "/live"), member)
	closeStream()

	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path(c, "/live"), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Cf-Access-Authenticated-User-Email", stranger)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("requesting stream as stranger: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("stranger stream = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct == "text/event-stream" {
		t.Errorf("stranger got Content-Type: text/event-stream, want an ordinary response")
	}
}

// The stream's opening contract, checked directly rather than only through
// openStream's internals: EventSource requires the SSE content type, and
// every other test in this file depends on the literal "retry: 3000"
// preamble existing as the sync barrier described on openStream.
func TestStreamContentTypeAndPreamble(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path(c, "/live"), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Cf-Access-Authenticated-User-Email", member)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("opening stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading preamble: %v", err)
	}
	if line != "retry: 3000\n" {
		t.Errorf("first line = %q, want %q", line, "retry: 3000\n")
	}
}

// The whole point of live updates: a mutation made by one browser must reach
// another browser's open stream. Creating publishes the whole-list "items"
// event rather than a per-item one, because a new undone item belongs at the
// end of the undone group — above any done items — so the client cannot just
// append it and has to re-fetch the list.
func TestItemCreateReachesStream(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {"bread"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("creating item = %d, want 200", rec.Code)
	}

	if ev := mustReadEventOfKind(t, r, "items"); ev.Kind != "items" {
		t.Fatalf("event = %+v, want kind=items", ev)
	}
}

// Suppression of a browser's own echo happens client-side (live.js compares
// ev.origin against its own tab id), but that only works at all if the
// server stamps the mutating request's X-Live-Origin header back onto the
// event it publishes. This is the server half of that contract.
func TestOriginIsStampedOnEvent(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	const tab = "test-tab-id"
	rec := doOrigin(t, h, http.MethodPost, path(c, "/items"), owner, tab, url.Values{"title": {"bread"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("creating item = %d, want 200", rec.Code)
	}

	ev := mustReadEventOfKind(t, r, "items")
	if ev.Origin != tab {
		t.Errorf("event origin = %q, want %q", ev.Origin, tab)
	}
}

// Setting a cover has no fragment of its own to swap — live.js's "cover" case
// just reloads the page (see the comment beside it) — but it still has to
// reach a second browser at all before that reload can happen.
func TestCoverUploadReachesCollectionStream(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), owner, smallJPEG(t, 1))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover = %d, want 204", rec.Code)
	}

	ev := mustReadEventOfKind(t, r, "collection")
	if ev.Action != "cover" {
		t.Errorf("event = %+v, want action=cover", ev)
	}
}

// Removing a cover publishes the same event shape as setting one: the client
// side doesn't distinguish "there's a new banner" from "the banner is gone",
// it just reloads either way.
func TestCoverDeleteReachesCollectionStream(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	rec := do(t, h, http.MethodDelete, path(c, "/cover"), owner, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deleting cover = %d, want 204", rec.Code)
	}

	ev := mustReadEventOfKind(t, r, "collection")
	if ev.Action != "cover" {
		t.Errorf("event = %+v, want action=cover", ev)
	}
}

// Same contract as TestOriginIsStampedOnEvent, for the cover upload: live.js
// filters an event by comparing ev.origin against its own tab id, which only
// works if the server stamps the mutating request's X-Live-Origin header back
// onto the event it publishes. This is the server half of that contract for
// handleUploadCollectionCover.
func TestCoverOriginIsStampedOnEvent(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	const tab = "test-cover-tab"
	rec := doMultipartOrigin(t, h, http.MethodPut, path(c, "/cover"), owner, tab, smallJPEG(t, 2))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover = %d, want 204", rec.Code)
	}

	ev := mustReadEventOfKind(t, r, "collection")
	if ev.Origin != tab {
		t.Errorf("event origin = %q, want %q", ev.Origin, tab)
	}
}

// Toggling and deleting are the two other item mutations besides create, and
// they publish deliberately different shapes. A tick moves the row — done
// items sort to the bottom — so it can only be communicated as a whole-list
// event. A delete leaves every other row where it was, so it stays a per-item
// event carrying the id, which is what lets the client drop that one row
// without disturbing an edit form open elsewhere on the page.
func TestToggleAndDeleteEvents(t *testing.T) {
	h, db := newTestServer(t)
	c, item := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	itemPath := path(c, "/items/"+strconv.FormatInt(item.ID, 10))

	if rec := do(t, h, http.MethodPost, itemPath+"/toggle", owner, nil); rec.Code != http.StatusOK {
		t.Fatalf("toggling item = %d, want 200", rec.Code)
	}
	if ev := mustReadEventOfKind(t, r, "items"); ev.Kind != "items" {
		t.Errorf("toggle event = %+v, want kind=items", ev)
	}

	if rec := do(t, h, http.MethodDelete, itemPath, owner, nil); rec.Code != http.StatusOK {
		t.Fatalf("deleting item = %d, want 200", rec.Code)
	}
	if ev := mustReadEventOfKind(t, r, "item"); ev.Action != "deleted" || ev.ID != item.ID {
		t.Errorf("delete event = %+v, want action=deleted id=%d", ev, item.ID)
	}
}

// Inviting someone changes the People list for everyone already looking at
// it, not just the person invited.
func TestAddMemberReachesCollectionStream(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	rec := do(t, h, http.MethodPost, path(c, "/members"), owner, url.Values{"email": {"third@example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding member = %d, want 200", rec.Code)
	}

	mustReadEventOfKind(t, r, "members") // fails the test itself if it never arrives
}

// This is the ordering that matters most in the whole design: handleRemoveMember
// publishes the "access"/"revoked" event before it evicts the removed
// member's subscriber (routes.go), and Evict never drains a subscriber's
// already-buffered messages before closing its channel (hub.go) — so the
// event is guaranteed to still be sitting in the buffer when Evict runs. If
// that ordering ever regressed to evict-then-publish, this test would see
// the stream end with no access event ever having arrived, and fail for a
// real reason rather than a timing fluke.
func TestRemoveMemberRevokesAndCloses(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	srv := httptest.NewServer(h)
	defer srv.Close()

	m, err := db.UserByEmail(context.Background(), member)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}

	// This is the member's own collection stream — the one connection that
	// must both receive the revocation and then be closed.
	r, closeStream := openStream(t, srv, path(c, "/live"), member)
	defer closeStream()

	target := path(c, "/members/"+strconv.FormatInt(m.ID, 10))
	if rec := do(t, h, http.MethodDelete, target, owner, nil); rec.Code != http.StatusOK {
		t.Fatalf("removing member = %d, want 200", rec.Code)
	}

	// Drain every event until the stream actually closes, rather than
	// assuming the revoked event is first or last among however many
	// handleRemoveMember happens to publish — the property under test is
	// "it arrives before close", not "it arrives at position N".
	var gotRevoked bool
	for {
		ev, err := readEvent(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v, want io.EOF", err)
			}
			break
		}
		if ev.Kind == "access" {
			gotRevoked = true
			if ev.Action != "revoked" || ev.Collection != c.ID {
				t.Errorf("access event = %+v, want action=revoked collection=%d", ev, c.ID)
			}
		}
	}
	if !gotRevoked {
		t.Fatal("stream closed without ever delivering the access/revoked event")
	}
}

// The members fragment gates the remove button on IsOwner ({{if $.IsOwner}}
// in fragments.html). handleMembersFragment is reachable by any member, not
// just the owner, so this guards the one thing that would leak an
// owner-only control to a plain member: unlike renderMembers (only reachable
// from owner-only routes, so it hardcodes IsOwner: true), this route must
// compute it per viewer.
func TestMembersFragmentPerViewer(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodGet, path(c, "/members"), owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner fetching members = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "person_remove") {
		t.Error("owner's members fragment is missing the remove control")
	}

	rec = do(t, h, http.MethodGet, path(c, "/members"), member, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member fetching members = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "person_remove") {
		t.Error("a plain member's members fragment leaks the owner-only remove control")
	}
}

// main.go relies on Server.Close() making every open stream's handler return
// promptly — that's what lets graceful shutdown skip waiting out the full
// 10s Shutdown timeout for live connections (see main.go's comment beside
// the Close() call, and web.go's Close doc comment). This drives Close()
// itself rather than a real SIGTERM, so it stays deterministic: no process
// signalling, just the same call main.go makes.
func TestCloseEndsOpenStream(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv := New(db, "")
	c, _ := fixture(t, db)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	r, closeStream := openStream(t, httpSrv, path(c, "/live"), member)
	defer closeStream()

	srv.Close()

	if _, err := readEvent(r); !errors.Is(err, io.EOF) {
		t.Fatalf("stream after Server.Close() ended with %v, want io.EOF", err)
	}
}
