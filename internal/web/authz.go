package web

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"list/internal/store"
)

// The collection and item ids in every URL below are entirely
// attacker-controlled — they are just integers someone can type. The two
// helpers in this file are the whole authorisation model: every handler that
// touches a collection goes through one of them, and nothing else is
// permitted to load a collection by id.

// collectionFor resolves the {collection} path value, confirms the caller may
// see it, and optionally confirms they own it.
//
// A non-member gets 404, not 403. A 403 would confirm that a collection with
// that id exists, which lets someone enumerate other people's lists even
// though they cannot read them. A member who is merely not the owner does get
// 403, because they already know it exists.
func (s *Server) collectionFor(w http.ResponseWriter, r *http.Request, needOwner bool) (store.User, store.Collection, bool) {
	u, ok := currentUser(w, r)
	if !ok {
		return store.User{}, store.Collection{}, false
	}

	id, err := strconv.ParseInt(r.PathValue("collection"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.User{}, store.Collection{}, false
	}

	c, err := s.store.Collection(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.User{}, store.Collection{}, false
	}
	if err != nil {
		log.Printf("collectionFor: Collection(%d): %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.User{}, store.Collection{}, false
	}

	member, err := s.store.IsMember(r.Context(), id, u.ID)
	if err != nil {
		log.Printf("collectionFor: IsMember(%d, %d): %v", id, u.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.User{}, store.Collection{}, false
	}
	if !member {
		http.NotFound(w, r)
		return store.User{}, store.Collection{}, false
	}

	if needOwner && c.OwnerID != u.ID {
		http.Error(w, "only the owner can do that", http.StatusForbidden)
		return store.User{}, store.Collection{}, false
	}

	return u, c, true
}

// itemFor resolves {item} within an already-authorised collection.
//
// The item's own collection_id is re-checked against the collection from the
// URL. Without that, membership of any one collection would be enough to edit
// an item in any other, simply by pairing a collection id you can see with an
// item id you cannot — the membership check alone does not cover it.
func (s *Server) itemFor(w http.ResponseWriter, r *http.Request, c store.Collection) (store.ItemView, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.ItemView{}, false
	}

	item, err := s.store.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.ItemView{}, false
	}
	if err != nil {
		log.Printf("itemFor: Item(%d): %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.ItemView{}, false
	}
	if item.CollectionID != c.ID {
		http.NotFound(w, r)
		return store.ItemView{}, false
	}
	return item, true
}
