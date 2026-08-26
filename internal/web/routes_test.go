package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
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

// doMultipart is do()'s counterpart for the cover upload, the one route that
// takes a real file rather than a urlencoded form. It wraps data in a single
// part named "cover" — the form name handleUploadCollectionCover requires —
// under the multipart/form-data encoding htmx actually sends for it (see the
// plan's note on hx-encoding).
func doMultipart(t *testing.T, h http.Handler, method, path, email string, data []byte) *httptest.ResponseRecorder {
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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// smallJPEG builds a small, valid JPEG from a solid generated image — good
// enough to exercise the decode/re-encode pipeline without a real photo.
// seed varies the pixel colours so two calls produce different bytes, and
// therefore different etags, which the cross-collection test below needs.
func smallJPEG(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{seed, uint8(x * 16), uint8(y * 16), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// jpegWithEXIF builds a valid JPEG and splices an APP1 segment carrying a
// fake EXIF blob right after the SOI marker — the shape a real phone photo
// takes, minus the actual camera. It exists to prove the server's re-encode
// strips it, rather than trusting the browser's own canvas step to have done
// so — see "Stripping metadata" in the plan.
func jpegWithEXIF(t *testing.T) []byte {
	t.Helper()
	base := smallJPEG(t, 42)

	payload := append([]byte("Exif\x00\x00"), bytes.Repeat([]byte{0xAB}, 32)...)
	length := len(payload) + 2 // the JPEG segment length includes itself
	segment := []byte{0xFF, 0xE1, byte(length >> 8), byte(length)}
	segment = append(segment, payload...)

	out := make([]byte, 0, len(base)+len(segment))
	out = append(out, base[:2]...) // SOI
	out = append(out, segment...)  // spliced-in APP1/EXIF
	out = append(out, base[2:]...)
	return out
}

// pngClaimingHugeDimensions builds a real, tiny PNG and then rewrites its
// IHDR chunk's width/height (recomputing the chunk CRC to match) so the
// header claims dimensions with nothing to do with the handful of bytes that
// actually follow — exactly the shape of a decompression-bomb file. A
// non-paletted PNG's DecodeConfig returns as soon as IHDR is parsed, so
// nothing after byte 33 (8-byte signature + 25-byte IHDR chunk) is ever read
// to produce that config, however implausible the rest of the file is.
func pngClaimingHugeDimensions(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	out := append([]byte(nil), buf.Bytes()[:33]...)
	binary.BigEndian.PutUint32(out[16:20], 30000) // width
	binary.BigEndian.PutUint32(out[20:24], 30000) // height
	crc := crc32.ChecksumIEEE(out[12:29])         // "IHDR" + its 13 data bytes
	binary.BigEndian.PutUint32(out[29:33], crc)
	return out
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

// extractDiv returns the inner content of the first <div class="class">…
// </div> in s. Good enough for a test that only needs one fragment's markup,
// without pulling an HTML parser in just to look inside a single element.
func extractDiv(s, class string) (string, bool) {
	open := `<div class="` + class + `">`
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// stripTags removes every "<…>" run from s, leaving only its text nodes —
// enough to recover linkedText's visible output from around the <a> tags it
// may have introduced, which is all TestItemBodyWhitespaceIsPreserved below
// needs.
func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
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
		{http.MethodGet, path(c, "/cover/deadbeef"), nil},
		{http.MethodPut, path(c, "/cover"), nil},
		{http.MethodDelete, path(c, "/cover"), nil},
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
	// The cover follows the same any-member bar as everything else here — see
	// collectionFor(w, r, false) on all three cover routes.
	if rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, smallJPEG(t, 1)); rec.Code != http.StatusNoContent {
		t.Errorf("member uploading cover = %d, want 204", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, path(c, "/cover"), member, nil); rec.Code != http.StatusNoContent {
		t.Errorf("member deleting cover = %d, want 204", rec.Code)
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

// A member may reorder items on a collection they belong to — the same bar
// as every other item mutation — and a stranger gets the usual 404 rather
// than a 403 that would confirm the collection exists.
func TestReorderItems(t *testing.T) {
	h, db := newTestServer(t)
	c, item := fixture(t, db)

	second, err := db.CreateItem(context.Background(), c.ID, c.OwnerID, "bread", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	order := strconv.FormatInt(second.ID, 10) + "," + strconv.FormatInt(item.ID, 10)
	rec := do(t, h, http.MethodPost, path(c, "/items/reorder"), member, url.Values{"order": {order}})
	if rec.Code != http.StatusOK {
		t.Fatalf("member reordering = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="items"`) {
		t.Errorf("reorder response missing the items fragment: %s", rec.Body.String())
	}

	items, err := db.Items(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 2 || items[0].ID != second.ID || items[1].ID != item.ID {
		t.Fatalf("order after reorder = %+v, want [%d, %d]", items, second.ID, item.ID)
	}

	rec = do(t, h, http.MethodPost, path(c, "/items/reorder"), stranger, url.Values{"order": {order}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger reordering = %d, want 404", rec.Code)
	}
}

// A malformed order value — anything that is not a comma-separated list of
// integers — is a validation failure, not a server error.
func TestReorderItemsRejectsMalformedOrder(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	cases := []struct {
		name  string
		order string
	}{
		{"empty", ""},
		{"not a number", "abc"},
		{"trailing comma with junk", "1,2,xyz"},
	}
	for _, tc := range cases {
		rec := do(t, h, http.MethodPost, path(c, "/items/reorder"), owner, url.Values{"order": {tc.order}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: order=%q = %d, want 422", tc.name, tc.order, rec.Code)
		}
	}
}

// Ticking an item moves it in the render order (done items sink to the
// bottom), so the response has to be the whole list, not the single row the
// old per-item fragment returned — otherwise the row would stay stuck in its
// old spot in the DOM until the next full refresh.
func TestToggleReturnsWholeList(t *testing.T) {
	h, db := newTestServer(t)
	c, item := fixture(t, db)
	itemPath := path(c, "/items/"+strconv.FormatInt(item.ID, 10))

	rec := do(t, h, http.MethodPost, itemPath+"/toggle", owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggling = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="items"`) {
		t.Errorf("toggle response is not the whole items fragment: %s", rec.Body.String())
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

// A URL in a title becomes a real link, with the new-tab and no-referrer
// attributes the plan settled on: you keep your place in the list, and the
// destination is never told which app the click came from.
func TestItemTitleIsLinkified(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {"check https://example.com now"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	want := `<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("title anchor missing from output: %s", rec.Body.String())
	}
}

// The body goes through the same "linkedText" define as the title, so it
// gets the same link.
func TestItemBodyIsLinkified(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {"ok"}, "body": {"see https://example.com here"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	want := `<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body anchor missing from output: %s", rec.Body.String())
	}
}

// Linkifying a field must not weaken its escaping — a <script> payload
// alongside a URL in the same field still comes out neutralised, and the URL
// still becomes a link.
func TestLinkifyDoesNotWeakenEscaping(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {`<script>alert(1)</script> https://example.com`}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("script tag was not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected the escaped form in the output: %s", body)
	}
	if !strings.Contains(body, `<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>`) {
		t.Errorf("URL in the same field as the script payload was not linkified: %s", body)
	}
}

// A bare "www." address gets an "https://" href, but the displayed text
// stays exactly as typed — see linkify's doc comment on why Text and URL can
// differ for that one prefix.
func TestBareWWWGetsHTTPSHref(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodPost, path(c, "/items"), owner, url.Values{"title": {"www.example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("adding item = %d, want 200", rec.Code)
	}
	want := `<a href="https://www.example.com" target="_blank" rel="noopener noreferrer">www.example.com</a>`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("www anchor missing or wrong in output: %s", rec.Body.String())
	}
}

// itemForm (fragments.html) uses attribute and RCDATA contexts and was
// deliberately never wired to "linkedText" — the edit form must keep
// showing raw text, both so Save round-trips the actual value and so a URL
// never turns into markup a person could not then edit as text.
func TestEditFormIsNotLinkified(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	ctx := context.Background()
	o, err := db.UserByEmail(ctx, owner)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	item, err := db.CreateItem(ctx, c.ID, o.ID, "check https://example.com", "")
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	rec := do(t, h, http.MethodGet, path(c, "/items/"+strconv.FormatInt(item.ID, 10)+"/edit"), owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("editing item = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://example.com") {
		t.Errorf("edit form does not contain the raw URL: %s", body)
	}
	if strings.Contains(body, "<a") {
		t.Errorf("edit form was linkified: %s", body)
	}
}

// The regression this whole feature is built to avoid: text/template emits
// the "linkedText" define's body byte for byte, so a newline or leading
// indentation introduced by someone tidying that line would show up as a
// real line break in the middle of a pre-wrap note. This renders a body
// with both a literal newline and a link, strips the tags linkedText can
// introduce, and requires the result to be the input verbatim — not merely
// "no obviously wrong whitespace", but exactly what was typed.
func TestItemBodyWhitespaceIsPreserved(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)
	ctx := context.Background()
	o, err := db.UserByEmail(ctx, owner)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	const body = "a\nb https://x.com c"
	item, err := db.CreateItem(ctx, c.ID, o.ID, "whitespace check", body)
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	rec := do(t, h, http.MethodGet, path(c, "/items/"+strconv.FormatInt(item.ID, 10)), owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching item = %d, want 200", rec.Code)
	}

	inner, ok := extractDiv(rec.Body.String(), "item-body small-text")
	if !ok {
		t.Fatalf("no item-body div in output: %s", rec.Body.String())
	}
	if got := stripTags(inner); got != body {
		t.Errorf("item-body text = %q, want %q (rendered div: %q)", got, body, inner)
	}
}

// ------------------------------------------------------------------- covers

// An upload past maxCoverBytes is refused before any image work happens on
// it. The data itself is not a valid image at all — size is what this test
// means to exercise, and the size check runs before the content sniff.
func TestCoverUploadRejectsOversize(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	data := bytes.Repeat([]byte("a"), maxCoverBytes+1024)
	rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, data)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize cover upload = %d, want 413", rec.Code)
	}
}

// Something that is not an image at all is refused by the sniff check, well
// before any decode is attempted.
func TestCoverUploadRejectsNonImage(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, []byte("just some text, not a picture"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-image cover upload = %d, want 422", rec.Code)
	}
}

// A small file that merely declares an enormous width and height is rejected
// by the DecodeConfig dimension guard, before image.Decode ever runs and
// tries to allocate for it — see maxCoverEdge/maxCoverPixels in routes.go.
func TestCoverUploadRejectsHugeDimensions(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, pngClaimingHugeDimensions(t))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cover claiming huge dimensions = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("rejection does not mention the dimensions: %s", rec.Body.String())
	}
}

// The test that matters most in this step: a JPEG carrying a spliced-in
// APP1/EXIF segment comes back from the serving route with no APPn segment
// of any kind, proving the server's decode/re-encode is what strips it — not
// the browser's canvas step, which this request bypasses entirely.
func TestCoverUploadStripsEXIF(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	upload := jpegWithEXIF(t)
	if !bytes.Contains(upload, []byte{0xFF, 0xE1}) {
		t.Fatal("test fixture does not actually contain an APP1 marker")
	}

	rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, upload)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("uploading a cover with EXIF = %d, want 204", rec.Code)
	}

	etag, _, _, err := db.CollectionImageETag(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("CollectionImageETag: %v", err)
	}
	if etag == "" {
		t.Fatal("no cover etag stored after upload")
	}

	rec = do(t, h, http.MethodGet, path(c, "/cover/"+etag), member, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching the stored cover = %d, want 200", rec.Code)
	}

	stored := rec.Body.Bytes()
	if bytes.Contains(stored, []byte{0xFF, 0xE1}) {
		t.Error("stored cover still contains an APP1 marker")
	}
	for i := 0; i < len(stored)-1; i++ {
		if stored[i] == 0xFF && stored[i+1] >= 0xE0 && stored[i+1] <= 0xEF {
			t.Fatalf("stored cover contains an APPn marker (0xFF 0x%02X) at byte %d", stored[i+1], i)
		}
	}
}

// A served cover carries the type actually stored (never anything a client
// could have claimed), nosniff, and the cacheImmutable override rather than
// the no-store default.
func TestCoverServingHeaders(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	if rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, smallJPEG(t, 7)); rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover = %d, want 204", rec.Code)
	}
	etag, _, _, err := db.CollectionImageETag(context.Background(), c.ID)
	if err != nil || etag == "" {
		t.Fatalf("CollectionImageETag: etag=%q err=%v", etag, err)
	}

	rec := do(t, h, http.MethodGet, path(c, "/cover/"+etag), member, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetching cover = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "private") || !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want it to contain private and immutable", cc)
	}
	if strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, must not carry no-store", cc)
	}
}

// authenticate's no-store default must still reach an ordinary route
// unchanged — pinning this is what keeps the cacheImmutable override an
// exception rather than something that quietly spreads.
func TestOrdinaryRouteStillCarriesNoStore(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	rec := do(t, h, http.MethodGet, path(c, ""), member, nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control on an ordinary route = %q, want no-store", got)
	}
}

// A path {etag} that does not match what is actually stored 404s rather than
// silently serving whatever the current cover happens to be — the property
// that makes the immutable caching directive on a match truthful.
func TestCoverWrongEtagNotFound(t *testing.T) {
	h, db := newTestServer(t)
	c, _ := fixture(t, db)

	if rec := doMultipart(t, h, http.MethodPut, path(c, "/cover"), member, smallJPEG(t, 9)); rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover = %d, want 204", rec.Code)
	}
	etag, _, _, err := db.CollectionImageETag(context.Background(), c.ID)
	if err != nil || etag == "" {
		t.Fatalf("CollectionImageETag: etag=%q err=%v", etag, err)
	}
	wrong := etag[:len(etag)-1] + "0"
	if wrong == etag {
		wrong = etag[:len(etag)-1] + "1"
	}

	rec := do(t, h, http.MethodGet, path(c, "/cover/"+wrong), member, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("wrong etag = %d, want 404", rec.Code)
	}
}

// A collection's cover is not reachable by naming a different collection's
// URL, even by a caller who is a legitimate member of that other collection —
// the {collection} and {etag} segments are checked together, not separately.
func TestCoverNotReachableThroughAnotherCollection(t *testing.T) {
	h, db := newTestServer(t)
	ctx := context.Background()
	c1, _ := fixture(t, db)

	o, err := db.UserByEmail(ctx, owner)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	c2, err := db.CreateCollection(ctx, o.ID, "other")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := db.AddMember(ctx, c2.ID, member); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if rec := doMultipart(t, h, http.MethodPut, path(c1, "/cover"), owner, smallJPEG(t, 11)); rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover to c1 = %d, want 204", rec.Code)
	}
	if rec := doMultipart(t, h, http.MethodPut, path(c2, "/cover"), owner, smallJPEG(t, 200)); rec.Code != http.StatusNoContent {
		t.Fatalf("uploading cover to c2 = %d, want 204", rec.Code)
	}

	etag1, _, _, err := db.CollectionImageETag(ctx, c1.ID)
	if err != nil || etag1 == "" {
		t.Fatalf("CollectionImageETag(c1): etag=%q err=%v", etag1, err)
	}
	etag2, _, _, err := db.CollectionImageETag(ctx, c2.ID)
	if err != nil || etag2 == "" {
		t.Fatalf("CollectionImageETag(c2): etag=%q err=%v", etag2, err)
	}
	if etag1 == etag2 {
		t.Fatal("test fixture produced identical etags for two different covers")
	}

	rec := do(t, h, http.MethodGet, path(c2, "/cover/"+etag1), member, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("c1's cover through c2's URL = %d, want 404", rec.Code)
	}
}
