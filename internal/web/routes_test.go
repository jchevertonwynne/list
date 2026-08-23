package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"list/internal/store"
)

// These tests run against a real SQLite database rather than a fake store.
// The thing most worth testing here is the authorisation matrix, and that is
// enforced partly by store queries (IsMember) and partly by handlers — a fake
// would let a wrong answer from either half pass unnoticed.

const (
	owner    = "owner@example.com"
	member   = "member@example.com"
	stranger = "stranger@example.com"
)

func newTestServer(t *testing.T) (http.Handler, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, "").Handler(), db
}

// do issues a request as the given email. Mutations get the HX-Request header
// the CSRF guard requires; see csrfTest for the case where it is missing.
func do(t *testing.T, h http.Handler, method, path, email string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Cf-Access-Authenticated-User-Email", email)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		r.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// fixture builds a collection owned by owner, shared with member, holding one
// item.
func fixture(t *testing.T, db *store.DB) (store.Collection, store.ItemView) {
	t.Helper()
	ctx := context.Background()
	o, err := db.UserByEmail(ctx, owner)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c, err := db.CreateCollection(ctx, o.ID, "groceries")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := db.AddMember(ctx, c.ID, member); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	item, err := db.CreateItem(ctx, c.ID, o.ID, "milk", "semi-skimmed")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return c, item
}

func path(c store.Collection, suffix string) string {
	return "/collections/" + strconv.FormatInt(c.ID, 10) + suffix
}

// A stranger must not be able to tell that a collection exists at all. Every
// route answers 404, never 403: a 403 confirms the id is real and turns the
// URL space into a directory of other people's lists.
func TestStrangerSeesNothing(t *testing.T) {
	h, db := newTestServer(t)
	c, item := fixture(t, db)
	itemPath := path(c, "/items/"+strconv.FormatInt(item.ID, 10))

	cases := []struct {
		method, url string
		form        url.Values
	}{
		{http.MethodGet, path(c, ""), nil},
		{http.MethodGet, itemPath, nil},
		{http.MethodGet, itemPath + "/edit", nil},
		{http.MethodPost, path(c, "/items"), url.Values{"title": {"bread"}}},
		{http.MethodPost, path(c, "/rename"), url.Values{"name": {"stolen"}}},
		{http.MethodDelete, path(c, ""), nil},
		{http.MethodPost, path(c, "/members"), url.Values{"email": {"x@example.com"}}},
		{http.MethodPut, itemPath, url.Values{"title": {"nope"}}},
		{http.MethodPost, itemPath + "/toggle", nil},
		{http.MethodDelete, itemPath, nil},
	}
	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.url, stranger, tc.form)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as stranger = %d, want 404", tc.method, tc.url, rec.Code)
		}
	}
}

// A member may work on the list but not administer it. These get 403 rather
// than 404 because they already know the collection exists.
func TestMemberCannotAdminister(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	cases := []struct {
		method, url string
		form        url.Values
	}{
		{http.MethodPost, path(c, "/rename"), url.Values{"name": {"renamed"}}},
		{http.MethodDelete, path(c, ""), nil},
		{http.MethodPost, path(c, "/members"), url.Values{"email": {"x@example.com"}}},
	}
	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.url, member, tc.form)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as member = %d, want 403", tc.method, tc.url, rec.Code)
		}
	}
}

// The counterpart: a member genuinely can edit the contents, including items
// somebody else created.
func TestMemberCanEditItems(t *testing.T) {
	h, db := newTestServer(t)
	c, item := fixture(t, db)
	itemPath := path(c, "/items/"+strconv.FormatInt(item.ID, 10))

	if rec := do(t, h, http.MethodGet, path(c, ""), member, nil); rec.Code != http.StatusOK {
		t.Fatalf("member viewing collection = %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, path(c, "/items"), member, url.Values{"title": {"bread"}}); rec.Code != http.StatusOK {
		t.Errorf("member adding item = %d, want 200", rec.Code)
	}
	// The item was created by the owner, not this member.
	if rec := do(t, h, http.MethodPut, itemPath, member, url.Values{"title": {"oat milk"}}); rec.Code != http.StatusOK {
		t.Errorf("member editing another's item = %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, itemPath+"/toggle", member, nil); rec.Code != http.StatusOK {
		t.Errorf("member toggling = %d, want 200", rec.Code)
	}
}

// Pairing a collection you can see with an item you cannot must fail. The
// membership check alone does not cover this: the caller is a legitimate
// member of the collection named in the URL.
func TestItemFromAnotherCollectionIsNotFound(t *testing.T) {
	h, db := newTestServer(t)
	mine, _ := fixture(t, db)

	ctx := context.Background()
	other, err := db.UserByEmail(ctx, stranger)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	theirs, err := db.CreateCollection(ctx, other.ID, "private")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	secret, err := db.CreateItem(ctx, theirs.ID, other.ID, "secret", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	smuggled := path(mine, "/items/"+strconv.FormatInt(secret.ID, 10))
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if rec := do(t, h, m, smuggled, owner, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s cross-collection item = %d, want 404", m, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodPut, smuggled, owner, url.Values{"title": {"x"}}); rec.Code != http.StatusNotFound {
		t.Errorf("PUT cross-collection item = %d, want 404", rec.Code)
	}
}

// A mutation without HX-Request is what a cross-site form post looks like.
func TestCSRFGuard(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	r := httptest.NewRequest(http.MethodPost, path(c, "/items"), strings.NewReader("title=bread"))
	r.Header.Set("Cf-Access-Authenticated-User-Email", owner)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// deliberately no HX-Request
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without HX-Request = %d, want 403", rec.Code)
	}

	// Same request from another origin, with the header, is still refused.
	r = httptest.NewRequest(http.MethodPost, path(c, "/items"), strings.NewReader("title=bread"))
	r.Header.Set("Cf-Access-Authenticated-User-Email", owner)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	r.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", rec.Code)
	}
}

func TestOwnerCannotBeRemoved(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	target := path(c, "/members/"+strconv.FormatInt(c.OwnerID, 10))
	if rec := do(t, h, http.MethodDelete, target, owner, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("removing the owner = %d, want 422", rec.Code)
	}
}

func TestValidation(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	cases := []struct {
		name, url string
		form      url.Values
	}{
		{"empty collection name", "/collections", url.Values{"name": {"  "}}},
		{"long collection name", "/collections", url.Values{"name": {strings.Repeat("x", maxNameLen+1)}}},
		{"empty item title", path(c, "/items"), url.Values{"title": {" "}}},
		{"long item title", path(c, "/items"), url.Values{"title": {strings.Repeat("x", maxTitleLen+1)}}},
		{"long item body", path(c, "/items"), url.Values{"title": {"ok"}, "body": {strings.Repeat("x", maxBodyLen+1)}}},
		{"not an email", path(c, "/members"), url.Values{"email": {"nope"}}},
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodPost, tc.url, owner, tc.form)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", tc.name, rec.Code)
		}
	}
}

// The whole loop through the real handlers, ending with the item actually
// gone from the rendered page.
func TestFullLifecycle(t *testing.T) {
	h, _ := newTestServer(t)

	rec := do(t, h, http.MethodPost, "/collections", owner, url.Values{"name": {"weekend"}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("creating collection = %d, want 204", rec.Code)
	}
	redirect := rec.Header().Get("HX-Redirect")
	if redirect == "" {
		t.Fatal("no HX-Redirect after creating a collection")
	}

	rec = do(t, h, http.MethodPost, redirect+"/items", owner, url.Values{"title": {"book train"}, "body": {"before friday"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "book train") {
		t.Errorf("new item fragment does not contain the title: %s", rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, redirect, owner, nil)
	if !strings.Contains(rec.Body.String(), "book train") {
		t.Fatal("item missing from the collection page")
	}

	// The index should now show one item, none done.
	rec = do(t, h, http.MethodGet, "/", owner, nil)
	if !strings.Contains(rec.Body.String(), "weekend") {
		t.Error("collection missing from the index")
	}
	if !strings.Contains(rec.Body.String(), "0 of 1 done") {
		t.Errorf("index does not show the expected progress: %s", rec.Body.String())
	}
}

// html/template must escape a title someone typed, not execute it.
func TestItemTitleIsEscaped(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	const payload = `<script>alert(1)</script>`
	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {payload}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("title was not escaped: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Errorf("expected the escaped form in the output: %s", rec.Body.String())
	}
}
