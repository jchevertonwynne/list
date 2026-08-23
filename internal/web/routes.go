package web

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

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
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.handleIndex)

	mux.HandleFunc("POST /collections", s.handleCreateCollection)
	mux.HandleFunc("GET /collections/{collection}", s.handleCollection)
	mux.HandleFunc("POST /collections/{collection}/rename", s.handleRenameCollection)
	mux.HandleFunc("DELETE /collections/{collection}", s.handleDeleteCollection)

	mux.HandleFunc("POST /collections/{collection}/members", s.handleAddMember)
	mux.HandleFunc("DELETE /collections/{collection}/members/{user}", s.handleRemoveMember)

	mux.HandleFunc("POST /collections/{collection}/items", s.handleCreateItem)
	mux.HandleFunc("GET /collections/{collection}/items/{item}", s.handleItem)
	mux.HandleFunc("GET /collections/{collection}/items/{item}/edit", s.handleEditItem)
	mux.HandleFunc("PUT /collections/{collection}/items/{item}", s.handleUpdateItem)
	mux.HandleFunc("POST /collections/{collection}/items/{item}/toggle", s.handleToggleItem)
	mux.HandleFunc("DELETE /collections/{collection}/items/{item}", s.handleDeleteItem)
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

// field pulls one trimmed form value and length-checks it.
func field(r *http.Request, name string, max int) (string, bool) {
	v := strings.TrimSpace(r.PostFormValue(name))
	return v, len(v) <= max
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

	// Land the user in the thing they just made rather than back on an index
	// where they would have to find it.
	w.Header().Set("HX-Redirect", "/collections/"+strconv.FormatInt(c.ID, 10))
	w.WriteHeader(http.StatusNoContent)
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
	s.render(w, "collection.html", pageData{
		UserEmail:  u.Email,
		Title:      c.Name,
		Collection: c,
		Items:      items,
		Members:    members,
		IsOwner:    c.OwnerID == u.ID,
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
	if err := s.store.DeleteCollection(r.Context(), c.ID); err != nil {
		internalError(w, "handleDeleteCollection", err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
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

	if _, err := s.store.AddMember(r.Context(), c.ID, email); err != nil {
		internalError(w, "handleAddMember", err)
		return
	}
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

	item, err := s.store.CreateItem(r.Context(), c.ID, u.ID, title, body)
	if err != nil {
		internalError(w, "handleCreateItem", err)
		return
	}
	s.renderFragment(w, "item", itemCtx{Item: item, Collection: c})
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
	updated, err := s.store.ToggleItem(r.Context(), item.ID)
	if err != nil {
		internalError(w, "handleToggleItem", err)
		return
	}
	s.renderFragment(w, "item", itemCtx{Item: updated, Collection: c})
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
