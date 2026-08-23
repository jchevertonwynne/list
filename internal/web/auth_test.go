package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"list/internal/store"
)

// fakeStore implements Store. UserByEmail is the only method authenticate
// calls, so it is the only one with real behaviour; it records the email it
// was called with so tests can assert on normalisation, and returns whatever
// the test configured. Every other method just needs to exist to satisfy the
// interface.
type fakeStore struct {
	user     store.User
	err      error
	gotEmail string
}

func (f *fakeStore) UserByEmail(ctx context.Context, email string) (store.User, error) {
	f.gotEmail = email
	return f.user, f.err
}

func (f *fakeStore) CollectionsForUser(ctx context.Context, userID int64) ([]store.CollectionView, error) {
	return nil, nil
}

func (f *fakeStore) CreateCollection(ctx context.Context, ownerID int64, name string) (store.Collection, error) {
	return store.Collection{}, nil
}

func (f *fakeStore) Collection(ctx context.Context, id int64) (store.Collection, error) {
	return store.Collection{}, nil
}

func (f *fakeStore) IsMember(ctx context.Context, collectionID, userID int64) (bool, error) {
	return false, nil
}

func (f *fakeStore) RenameCollection(ctx context.Context, id int64, name string) error {
	return nil
}

func (f *fakeStore) DeleteCollection(ctx context.Context, id int64) error {
	return nil
}

func (f *fakeStore) Items(ctx context.Context, collectionID int64) ([]store.ItemView, error) {
	return nil, nil
}

func (f *fakeStore) CreateItem(ctx context.Context, collectionID, creatorID int64, title, body string) (store.ItemView, error) {
	return store.ItemView{}, nil
}

func (f *fakeStore) Item(ctx context.Context, id int64) (store.ItemView, error) {
	return store.ItemView{}, nil
}

func (f *fakeStore) UpdateItem(ctx context.Context, id int64, title, body string) (store.ItemView, error) {
	return store.ItemView{}, nil
}

func (f *fakeStore) ToggleItem(ctx context.Context, id int64) (store.ItemView, error) {
	return store.ItemView{}, nil
}

func (f *fakeStore) DeleteItem(ctx context.Context, id int64) error {
	return nil
}

func (f *fakeStore) Members(ctx context.Context, collectionID int64) ([]store.User, error) {
	return nil, nil
}

func (f *fakeStore) AddMember(ctx context.Context, collectionID int64, email string) (store.User, error) {
	return store.User{}, nil
}

func (f *fakeStore) RemoveMember(ctx context.Context, collectionID, userID int64) error {
	return nil
}

// downstreamSpy is the next handler in the chain. It records whether it ran
// at all — the security property under test is as much "did not call next"
// as it is "returned the right status" — and, when it does run, what
// identity it saw via userFrom.
type downstreamSpy struct {
	called bool
	user   store.User
}

func (d *downstreamSpy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.called = true
		d.user, _ = userFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func doRequest(s *Server, next http.Handler, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		req.Header.Set(accessEmailHeader, header)
	}
	rec := httptest.NewRecorder()
	s.authenticate(next).ServeHTTP(rec, req)
	return rec
}

func TestAuthenticateHeaderPresent(t *testing.T) {
	fs := &fakeStore{user: store.User{ID: 1, Email: "a@b.com"}}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "a@b.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !spy.called {
		t.Fatal("downstream handler was not called")
	}
	if spy.user != fs.user {
		t.Fatalf("user in context = %+v, want %+v", spy.user, fs.user)
	}
}

func TestAuthenticateHeaderAbsentNoDevUserRejects(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "")

	// This is the security property that matters most in this file: a
	// missing header with no devUser configured must never fall through to
	// an anonymous, shared identity.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if spy.called {
		t.Fatal("downstream handler was called, want it skipped")
	}
}

func TestAuthenticateHeaderAbsentDevUserSet(t *testing.T) {
	fs := &fakeStore{user: store.User{ID: 2, Email: "dev@local"}}
	s := New(fs, "dev@local")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !spy.called {
		t.Fatal("downstream handler was not called")
	}
	if spy.user != fs.user {
		t.Fatalf("user in context = %+v, want %+v", spy.user, fs.user)
	}
	if fs.gotEmail != "dev@local" {
		t.Fatalf("UserByEmail called with %q, want %q", fs.gotEmail, "dev@local")
	}
}

func TestAuthenticateNormalisesEmail(t *testing.T) {
	fs := &fakeStore{user: store.User{ID: 3, Email: "mixed@case.com"}}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "  Mixed@Case.COM  ")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if fs.gotEmail != "mixed@case.com" {
		t.Fatalf("UserByEmail called with %q, want %q", fs.gotEmail, "mixed@case.com")
	}
}

func TestAuthenticateHeaderWinsOverDevUser(t *testing.T) {
	fs := &fakeStore{user: store.User{ID: 4, Email: "header@b.com"}}
	s := New(fs, "dev@local")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "header@b.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if fs.gotEmail != "header@b.com" {
		t.Fatalf("UserByEmail called with %q, want %q", fs.gotEmail, "header@b.com")
	}
}

func TestAuthenticateStoreErrorIsNotLeaked(t *testing.T) {
	wantLeak := "supersecret dsn detail"
	fs := &fakeStore{err: &testError{wantLeak}}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "a@b.com")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if spy.called {
		t.Fatal("downstream handler was called, want it skipped")
	}
	if strings.Contains(rec.Body.String(), wantLeak) {
		t.Fatalf("response body leaked the raw store error: %q", rec.Body.String())
	}
}

func TestAuthenticateSetsCacheControlNoStore(t *testing.T) {
	fs := &fakeStore{user: store.User{ID: 1, Email: "a@b.com"}}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "a@b.com")

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store")
	}
}

func TestAuthenticateRejectionAlsoSetsCacheControlNoStore(t *testing.T) {
	fs := &fakeStore{}
	s := New(fs, "")
	spy := &downstreamSpy{}

	rec := doRequest(s, spy.handler(), "")

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store")
	}
}

// testError is a plain error whose message is deliberately something that
// must never reach a client response body.
type testError struct {
	msg string
}

func (e *testError) Error() string { return e.msg }
