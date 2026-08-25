package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png" // decoder only; every upload is re-encoded to jpeg on the way out
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"list/internal/live"
	"list/internal/store"
)

// Field limits. Nothing here is a security boundary — SQLite would store a
// megabyte of title quite happily — but a page that renders one is unusable,
// and rejecting at the edge of the handler keeps that out of the database
// where it would be someone else's problem to clean up.
const (
	maxNameLen  = 200
	maxTitleLen = 200
	maxBodyLen  = 4000
	maxEmailLen = 320
	// maxCoverBytes bounds a cover upload, both as the http.MaxBytesReader
	// limit on the whole request body and as the check on the single
	// multipart part read out of it (see handleUploadCollectionCover). A
	// couple of MB comfortably covers a phone photo re-encoded to JPEG by the
	// client, on a Pi with no writable /tmp to spill a larger one into.
	maxCoverBytes = 2 << 20 // 2 MiB
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)

	mux.HandleFunc("POST /collections", s.handleCreateCollection)
	mux.HandleFunc("GET /collections", s.handleCollectionsFragment)
	mux.HandleFunc("GET /collections/{collection}", s.handleCollection)
	mux.HandleFunc("POST /collections/{collection}/rename", s.handleRenameCollection)
	mux.HandleFunc("DELETE /collections/{collection}", s.handleDeleteCollection)

	// Any member, not just the owner, may set, replace or remove a cover —
	// the same bar as every item mutation, and a deliberate choice recorded
	// in the plan rather than an oversight: a shared household list has no
	// natural reason to make this owner-only.
	mux.HandleFunc("GET /collections/{collection}/cover/{etag}", s.handleCollectionCover)
	mux.HandleFunc("PUT /collections/{collection}/cover", s.handleUploadCollectionCover)
	mux.HandleFunc("DELETE /collections/{collection}/cover", s.handleDeleteCollectionCover)

	mux.HandleFunc("POST /collections/{collection}/members", s.handleAddMember)
	mux.HandleFunc("GET /collections/{collection}/members", s.handleMembersFragment)
	mux.HandleFunc("DELETE /collections/{collection}/members/{user}", s.handleRemoveMember)

	mux.HandleFunc("POST /collections/{collection}/items", s.handleCreateItem)
	mux.HandleFunc("POST /collections/{collection}/items/reorder", s.handleReorderItems)
	mux.HandleFunc("GET /collections/{collection}/items", s.handleItemsFragment)
	mux.HandleFunc("GET /collections/{collection}/items/{item}", s.handleItem)
	mux.HandleFunc("GET /collections/{collection}/items/{item}/edit", s.handleEditItem)
	mux.HandleFunc("PUT /collections/{collection}/items/{item}", s.handleUpdateItem)
	mux.HandleFunc("POST /collections/{collection}/items/{item}/toggle", s.handleToggleItem)
	mux.HandleFunc("DELETE /collections/{collection}/items/{item}", s.handleDeleteItem)

	// All GET, so guardCSRF exempts them and EventSource — which cannot set
	// headers — can open them directly.
	mux.HandleFunc("GET /live", s.handleUserLive)
	mux.HandleFunc("GET /collections/{collection}/live", s.handleCollectionLive)
}

// ------------------------------------------------------------------ helpers

// badRequest reports a validation failure. 422 rather than 400 so that the
// htmx side can tell "you typed something wrong", which belongs on the page,
// apart from "the request was malformed", which does not.
func badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusUnprocessableEntity)
}

func internalError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// cacheImmutable overrides authenticate's default Cache-Control: no-store for
// the one response class where that default is actively wrong: a collection
// cover, served from a content-addressed URL — handleCollectionCover 404s on
// any {etag} that does not match what is stored, which is what makes calling
// this response immutable true rather than aspirational. Caching it hard
// turns a per-page-load image fetch from "one INSERT ... ON CONFLICT DO
// NOTHING write transaction on single-writer SQLite" (UserByEmail, run by
// authenticate on every request) into one fetch per version, ever.
//
// This is safe only with all three of the following true, so treat any of
// them changing as a reason to reconsider this call, not just the header:
//   - the URL changes whenever the bytes do, so there is nothing to
//     revalidate and no cache-busting query param to manage;
//   - the caller has already been through collectionFor, so only a current
//     member of the collection ever reaches this response;
//   - "private" is load-bearing, not decorative: Cloudflare's edge sits in
//     front of this origin, and "public" would let that edge store a
//     member-only image for anyone behind it to receive.
//
// A revalidating scheme (no-cache plus ETag) was rejected: it would still
// cost a full authenticated round trip — that same UserByEmail write — on
// every page load, forever, even when the answer is always 304.
func cacheImmutable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
}

// field pulls one trimmed form value and length-checks it.
func field(r *http.Request, name string, max int) (string, bool) {
	v := strings.TrimSpace(r.PostFormValue(name))
	return v, len(v) <= max
}

// notifyIndexes tells every member of c that their own index page's progress
// count for it may have changed. HasUserSubscribers is checked first so a
// checkbox toggle — the hottest of the mutations that call this — costs
// nothing beyond that check when nobody has an index page open to notify.
//
// A failure here is logged and swallowed rather than surfaced: the mutation
// this is called after has already succeeded and already has its own
// response on the way, and any client that misses the notification re-syncs
// on its next reconnect regardless (see live.js's onopen handling).
func (s *Server) notifyIndexes(r *http.Request, c store.Collection, origin string) {
	if !s.live.HasUserSubscribers() {
		return
	}
	members, err := s.store.Members(r.Context(), c.ID)
	if err != nil {
		log.Printf("notifyIndexes: Members(%d): %v", c.ID, err)
		return
	}
	for _, m := range members {
		s.live.Publish(live.UserTopic(m.ID), live.Event{Kind: "collections", Origin: origin})
	}
}

// ------------------------------------------------------------- collections

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	cols, err := s.store.CollectionsForUser(r.Context(), u.ID)
	if err != nil {
		internalError(w, "handleIndex: CollectionsForUser", err)
		return
	}
	s.render(w, "index.html", pageData{
		UserEmail:   u.Email,
		Title:       "list",
		Collections: cols,
	})
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	name, ok := field(r, "name", maxNameLen)
	if !ok {
		badRequest(w, "that name is too long")
		return
	}
	if name == "" {
		badRequest(w, "a collection needs a name")
		return
	}

	c, err := s.store.CreateCollection(r.Context(), u.ID, name)
	if err != nil {
		internalError(w, "handleCreateCollection", err)
		return
	}
	s.live.Publish(live.UserTopic(u.ID), live.Event{Kind: "collections", Origin: liveOrigin(r)})

	// Land the user in the thing they just made rather than back on an index
	// where they would have to find it.
	w.Header().Set("HX-Redirect", "/collections/"+strconv.FormatInt(c.ID, 10))
	w.WriteHeader(http.StatusNoContent)
}

// handleCollectionsFragment re-renders the index's collections list on its
// own, for live.js to re-fetch into #collections on a "collections" event —
// an owner adds someone, someone is removed, a collection is created or
// deleted, or the tab was disconnected and is resyncing on reconnect.
func (s *Server) handleCollectionsFragment(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	cols, err := s.store.CollectionsForUser(r.Context(), u.ID)
	if err != nil {
		internalError(w, "handleCollectionsFragment: CollectionsForUser", err)
		return
	}
	s.renderFragment(w, "collections", pageData{Collections: cols})
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	u, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	items, err := s.store.Items(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleCollection: Items", err)
		return
	}
	members, err := s.store.Members(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleCollection: Members", err)
		return
	}
	// An empty etag means this collection has no cover, which is the ordinary
	// case, not an error — see CollectionImageETag. Using it rather than
	// CollectionImage keeps image bytes off this, the hottest read path a
	// collection has.
	coverETag, coverWidth, coverHeight, err := s.store.CollectionImageETag(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleCollection: CollectionImageETag", err)
		return
	}
	s.render(w, "collection.html", pageData{
		UserEmail:   u.Email,
		Title:       c.Name,
		Collection:  c,
		Items:       items,
		Members:     members,
		IsOwner:     c.OwnerID == u.ID,
		CoverETag:   coverETag,
		CoverWidth:  coverWidth,
		CoverHeight: coverHeight,
	})
}

func (s *Server) handleRenameCollection(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, true)
	if !ok {
		return
	}
	name, ok := field(r, "name", maxNameLen)
	if !ok {
		badRequest(w, "that name is too long")
		return
	}
	if name == "" {
		badRequest(w, "a collection needs a name")
		return
	}
	if err := s.store.RenameCollection(r.Context(), c.ID, name); err != nil {
		internalError(w, "handleRenameCollection", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "collection", Action: "renamed", Origin: origin})
	s.notifyIndexes(r, c, origin)
	// The name appears in the app bar, the page title and the heading;
	// refreshing is cheaper than three coordinated swaps.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, true)
	if !ok {
		return
	}
	// Membership rows are ON DELETE CASCADE (see the schema in store.go), so
	// the member list must be captured before DeleteCollection runs — asking
	// afterwards would find nobody left to notify.
	members, err := s.store.Members(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleDeleteCollection: Members", err)
		return
	}
	if err := s.store.DeleteCollection(r.Context(), c.ID); err != nil {
		internalError(w, "handleDeleteCollection", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "collection", Action: "deleted", Origin: origin})
	for _, m := range members {
		s.live.Publish(live.UserTopic(m.ID), live.Event{Kind: "collections", Origin: origin})
	}
	// Evict last: it closes subscriber channels without draining them, so
	// publishing first is what guarantees the "deleted" event above is still
	// delivered — it's already sitting in each subscriber's buffer by the
	// time Evict runs. Publishing after Evict would drop it entirely.
	s.live.Evict(live.CollectionTopic(c.ID), 0)

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------- covers

// handleCollectionCover serves a collection's cover image. It is deliberately
// simple: the {etag} path segment is compared against what is actually
// stored, and a mismatch is a 404 rather than a redirect to the current
// one — that comparison, not the Cache-Control header below, is what makes
// calling this URL immutable true rather than merely convenient. It also
// covers a cover reached through the wrong collection's URL: that
// collection's own stored etag, whatever it is, is not going to be the one in
// the path either.
func (s *Server) handleCollectionCover(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	img, err := s.store.CollectionImage(r.Context(), c.ID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		internalError(w, "handleCollectionCover: CollectionImage", err)
		return
	}
	if r.PathValue("etag") != img.ETag {
		http.NotFound(w, r)
		return
	}

	// The type served is the server's own record of what was stored, never
	// anything a client supplied — see the upload path below, which sniffs
	// and then unconditionally re-encodes to image/jpeg before this column is
	// ever written.
	w.Header().Set("Content-Type", img.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Only on this, the success path: setting it before either return above
	// would ship a year-long cacheable directive on a 404 body.
	cacheImmutable(w)
	w.Write(img.Bytes)
}

// maxCoverEdge and maxCoverPixels bound what image.DecodeConfig is allowed to
// report before handleUploadCollectionCover attempts a full decode.
// image.Decode allocates roughly width*height*4 bytes for whatever the
// header claims, so a two-megabyte file that merely declares a 30000x30000
// header is a real denial-of-service on a Pi — maxCoverBytes bounds the file
// on the wire, not the lie inside it.
const (
	maxCoverEdge   = 8000
	maxCoverPixels = 40_000_000 // ~40 megapixels; well past any real cover photo
)

// handleUploadCollectionCover accepts one image as a multipart part named
// "cover", decodes it, and re-encodes it as JPEG before storing it. The
// re-encode is the metadata strip described below, not a side effect of it.
func (s *Server) handleUploadCollectionCover(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCoverBytes)

	// r.MultipartReader() streams the part straight off the connection into
	// the buffer below — deliberately never ParseMultipartForm or FormFile.
	// ReadForm (mime/multipart/formdata.go) spills anything past its memory
	// limit to os.CreateTemp, and this container has readOnlyRootFilesystem
	// with no /tmp to spill into. The streaming reader has no such code path
	// at all, which is the point: "nothing ever spills" becomes a property of
	// which function is called, not a constant someone has to remember to
	// keep in sync. A later "simplification" to r.FormFile would silently
	// reintroduce that crash — and it falls back to Go's 32MB default when no
	// limit is set, so it would also quietly stop depending on maxCoverBytes
	// at all.
	mr, err := r.MultipartReader()
	if err != nil {
		badRequest(w, "expected a multipart cover upload")
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		badRequest(w, "expected a multipart cover upload")
		return
	}
	defer part.Close()
	if part.FormName() != "cover" {
		badRequest(w, `expected a form field named "cover"`)
		return
	}

	// Reading cap+1 bytes off the part is how an oversized upload is
	// detected: if the copy actually reaches maxCoverBytes+1, the part alone
	// was too big. The MaxBytesReader above is the backstop for the same
	// condition arriving a different way (the request body as a whole,
	// multipart overhead included, crossing the cap first) — its error also
	// means "too large", so both are answered with 413 below rather than one
	// producing a generic 400 depending on exactly where the cap fell.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(part, maxCoverBytes+1))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
			return
		}
		badRequest(w, "could not read the uploaded image")
		return
	}
	if n > maxCoverBytes {
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		return
	}
	data := buf.Bytes()

	// A cheap early reject before any decode. png and jpeg are the only
	// formats accepted: the server re-encodes every upload (see below), and
	// the stdlib has no encoder for webp or gif, so accepting either would
	// mean shipping it back out unstripped. svg falls out for free — it
	// sniffs as text/xml, never as an image — which matters because an svg
	// served from this origin would execute script in this origin.
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg":
	default:
		badRequest(w, "only PNG and JPEG images are accepted")
		return
	}

	// DecodeConfig reads only the header, before any full decode, so an
	// image that merely declares absurd dimensions is rejected here rather
	// than in image.Decode below — see the comment on maxCoverEdge above for
	// why that ordering matters.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		badRequest(w, "that does not look like a valid image")
		return
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > maxCoverEdge || cfg.Height > maxCoverEdge ||
		cfg.Width*cfg.Height > maxCoverPixels {
		badRequest(w, "that image's dimensions are too large")
		return
	}

	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		badRequest(w, "that does not look like a valid image")
		return
	}

	// This is the actual enforcement point for metadata stripping. Go's jpeg
	// encoder emits only SOI, DQT, SOF0, DHT, SOS and EOI — zero APPn
	// segments of any kind — so EXIF, GPS, an ICC profile, XMP and any
	// embedded thumbnail cannot survive this call regardless of what arrived.
	// The browser's own canvas step already discards all of it, but the
	// client is not a trust boundary: anyone with a session can curl a PUT
	// with an untouched phone photo.
	var out bytes.Buffer
	if err := jpeg.Encode(&out, decoded, nil); err != nil {
		internalError(w, "handleUploadCollectionCover: jpeg.Encode", err)
		return
	}
	encoded := out.Bytes()

	// The etag is derived from the re-encoded bytes, not the upload: those
	// are what actually end up stored and served, so they are what the URL
	// must address. A truncated hex prefix keeps the URL short; collision
	// risk over one collection's history of covers is not a real concern at
	// this length.
	sum := sha256.Sum256(encoded)
	etag := hex.EncodeToString(sum[:8])

	bounds := decoded.Bounds()
	if err := s.store.SetCollectionImage(r.Context(), c.ID, "image/jpeg", encoded, etag, bounds.Dx(), bounds.Dy()); err != nil {
		internalError(w, "handleUploadCollectionCover: SetCollectionImage", err)
		return
	}

	origin := liveOrigin(r)
	// Not notifyIndexes: the cover is a collection-page banner only, never
	// shown on the index, so nothing there needs to know it changed.
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "collection", Action: "cover", Origin: origin})

	// Matches handleRenameCollection: the banner's <img> src carries the new
	// etag, so the acting tab has to reload to see it rather than being
	// handed a fragment to swap in.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteCollectionCover removes a collection's cover. Idempotent: the
// store method already tolerates a call with no cover left to remove (see
// DeleteCollectionImage), and this handler passes that straight through as
// 204 rather than checking first — the remove control sits on a live page,
// and a double-click or two tabs racing must not surface an error for a state
// already reached.
func (s *Server) handleDeleteCollectionCover(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	if err := s.store.DeleteCollectionImage(r.Context(), c.ID); err != nil {
		internalError(w, "handleDeleteCollectionCover", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "collection", Action: "cover", Origin: origin})
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------- members

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, true)
	if !ok {
		return
	}
	email, ok := field(r, "email", maxEmailLen)
	if !ok {
		badRequest(w, "that address is too long")
		return
	}
	// Deliberately not a full address grammar: the only address that can ever
	// actually log in is one that also appears in this hostname's Cloudflare
	// Access policy, so this check exists to catch typos, not to be a filter.
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t") {
		badRequest(w, "that does not look like an email address")
		return
	}

	newMember, err := s.store.AddMember(r.Context(), c.ID, email)
	if err != nil {
		internalError(w, "handleAddMember", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "members", Origin: origin})
	s.live.Publish(live.UserTopic(newMember.ID), live.Event{Kind: "collections", Origin: origin})
	s.renderMembers(w, r, c)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, true)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The owner's membership row is what puts the collection on their own
	// index page. Removing it would hide the collection from the only person
	// who can delete it, leaving a row nobody can reach.
	if id == c.OwnerID {
		badRequest(w, "the owner cannot be removed")
		return
	}
	if err := s.store.RemoveMember(r.Context(), c.ID, id); err != nil {
		internalError(w, "handleRemoveMember", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "members", Origin: origin})
	s.live.Publish(live.UserTopic(id), live.Event{Kind: "access", Collection: c.ID, Action: "revoked"})
	s.live.Publish(live.UserTopic(id), live.Event{Kind: "collections", Origin: origin})
	// Publish before Evict: the "revoked" event above must already be sitting
	// in the removed member's buffer before their collection-topic subscriber
	// is closed, or it never gets delivered (see handleDeleteCollection for
	// the same ordering requirement).
	s.live.Evict(live.CollectionTopic(c.ID), id)
	s.renderMembers(w, r, c)
}

func (s *Server) renderMembers(w http.ResponseWriter, r *http.Request, c store.Collection) {
	members, err := s.store.Members(r.Context(), c.ID)
	if err != nil {
		internalError(w, "renderMembers: Members", err)
		return
	}
	s.renderFragment(w, "members", pageData{
		Collection: c,
		Members:    members,
		IsOwner:    true, // only an owner reaches either caller
	})
}

// handleMembersFragment re-renders the People list on its own, for live.js
// to re-fetch into #members on a "members" event. Unlike renderMembers
// above, this route is reachable by any member, not just the owner, so
// IsOwner has to be computed for whoever is actually looking rather than
// hardcoded — the members fragment gates the remove buttons on it, and a
// non-owner must not be handed markup for an action they cannot take.
func (s *Server) handleMembersFragment(w http.ResponseWriter, r *http.Request) {
	u, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	members, err := s.store.Members(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleMembersFragment: Members", err)
		return
	}
	s.renderFragment(w, "members", pageData{
		Collection: c,
		Members:    members,
		IsOwner:    c.OwnerID == u.ID,
	})
}

// ------------------------------------------------------------------- items

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	u, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	title, titleOK := field(r, "title", maxTitleLen)
	body, bodyOK := field(r, "body", maxBodyLen)
	if !titleOK {
		badRequest(w, "that title is too long")
		return
	}
	if !bodyOK {
		badRequest(w, "that note is too long")
		return
	}
	if title == "" {
		badRequest(w, "an item needs a title")
		return
	}

	if _, err := s.store.CreateItem(r.Context(), c.ID, u.ID, title, body); err != nil {
		internalError(w, "handleCreateItem", err)
		return
	}
	items, err := s.store.Items(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleCreateItem: Items", err)
		return
	}
	origin := liveOrigin(r)
	// A new item lands at the end of the undone group (see CreateItem in
	// store.go), which can sit above existing done items — the form's old
	// hx-swap="beforeend" against a single-row fragment would append it after
	// those done rows instead, so the whole list is re-rendered here.
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "items", Origin: origin})
	s.notifyIndexes(r, c, origin)
	s.renderFragment(w, "items", pageData{Collection: c, Items: items})
}

// handleItemsFragment re-renders the whole items list on its own, for
// live.js to full-resync #items on reconnect. Per-item add/update/delete
// events use handleItem below instead, so a live edit in progress elsewhere
// on the page survives — see that comment.
func (s *Server) handleItemsFragment(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	items, err := s.store.Items(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleItemsFragment: Items", err)
		return
	}
	s.renderFragment(w, "items", pageData{Collection: c, Items: items})
}

// handleItem re-renders a single row. It exists for the cancel button on the
// edit form: restoring a client-side copy of the row would show the values as
// they were when editing started, quietly discarding a change someone else
// made to the same item in the meantime. Re-fetching shows what is stored.
func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	item, ok := s.itemFor(w, r, c)
	if !ok {
		return
	}
	s.renderFragment(w, "item", itemCtx{Item: item, Collection: c})
}

func (s *Server) handleEditItem(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	item, ok := s.itemFor(w, r, c)
	if !ok {
		return
	}
	s.renderFragment(w, "itemForm", itemCtx{Item: item, Collection: c})
}

func (s *Server) handleUpdateItem(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	item, ok := s.itemFor(w, r, c)
	if !ok {
		return
	}
	title, titleOK := field(r, "title", maxTitleLen)
	body, bodyOK := field(r, "body", maxBodyLen)
	if !titleOK {
		badRequest(w, "that title is too long")
		return
	}
	if !bodyOK {
		badRequest(w, "that note is too long")
		return
	}
	if title == "" {
		badRequest(w, "an item needs a title")
		return
	}

	updated, err := s.store.UpdateItem(r.Context(), item.ID, title, body)
	if err != nil {
		internalError(w, "handleUpdateItem", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "item", ID: updated.ID, Action: "updated", Origin: origin})
	s.notifyIndexes(r, c, origin)
	s.renderFragment(w, "item", itemCtx{Item: updated, Collection: c})
}

func (s *Server) handleToggleItem(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	item, ok := s.itemFor(w, r, c)
	if !ok {
		return
	}
	if _, err := s.store.ToggleItem(r.Context(), item.ID); err != nil {
		internalError(w, "handleToggleItem", err)
		return
	}
	items, err := s.store.Items(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleToggleItem: Items", err)
		return
	}
	origin := liveOrigin(r)
	// A tick sinks the row below every undone item purely via Items' ORDER BY
	// (see ToggleItem in store.go, which deliberately never writes position),
	// so the single-row "item" fragment used by handleUpdateItem is not
	// enough here — the row has to move in the DOM, not just change in place
	// — and the whole list is re-rendered instead.
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "items", Origin: origin})
	s.notifyIndexes(r, c, origin)
	s.renderFragment(w, "items", pageData{Collection: c, Items: items})
}

// handleReorderItems applies a drag-and-drop reorder. Any member may call
// this, the same as every other item mutation — see collectionFor's
// needOwner=false.
//
// The order comes in as a comma-separated list of ids in the "order" form
// value rather than as repeated form fields, because that is the shape a
// single hidden input fed by the client's drag library produces without any
// JS-side serialisation help.
func (s *Server) handleReorderItems(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}

	raw := strings.TrimSpace(r.PostFormValue("order"))
	if raw == "" {
		badRequest(w, "order must not be empty")
		return
	}
	parts := strings.Split(raw, ",")
	// Bounds the transaction ReorderItems runs, in the same spirit as
	// maxTitleLen and friends above: this is a household to-do list, and
	// nothing legitimate ever drags a thousand rows at once. Not a security
	// boundary, just keeping unusable input out of the database.
	if len(parts) > 1000 {
		badRequest(w, "too many items to reorder")
		return
	}
	ids := make([]int64, len(parts))
	for i, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			badRequest(w, "order must be a comma-separated list of item ids")
			return
		}
		ids[i] = id
	}

	if err := s.store.ReorderItems(r.Context(), c.ID, ids); err != nil {
		internalError(w, "handleReorderItems", err)
		return
	}
	items, err := s.store.Items(r.Context(), c.ID)
	if err != nil {
		internalError(w, "handleReorderItems: Items", err)
		return
	}
	origin := liveOrigin(r)
	// "items" rather than a per-item event: every id in the drag may have
	// moved, not just one row, so there is no single item id to name.
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "items", Origin: origin})
	s.notifyIndexes(r, c, origin)
	// Render the fragment back to the actor rather than trusting the DOM
	// order the drag left behind — ReorderItems silently skips ids that
	// belong to another collection or no longer exist (see its comment in
	// store.go), so the server's view of the new order is authoritative and
	// may legitimately differ from what was dragged.
	s.renderFragment(w, "items", pageData{Collection: c, Items: items})
}

func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.collectionFor(w, r, false)
	if !ok {
		return
	}
	item, ok := s.itemFor(w, r, c)
	if !ok {
		return
	}
	if err := s.store.DeleteItem(r.Context(), item.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		internalError(w, "handleDeleteItem", err)
		return
	}
	origin := liveOrigin(r)
	s.live.Publish(live.CollectionTopic(c.ID), live.Event{Kind: "item", ID: item.ID, Action: "deleted", Origin: origin})
	s.notifyIndexes(r, c, origin)
	// An empty body with an outerHTML swap is how the row leaves the page.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// itemCtx is what the item fragments render against: a row needs its
// collection id to build its own action URLs.
type itemCtx struct {
	Item       store.ItemView
	Collection store.Collection
}
