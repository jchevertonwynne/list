// Package store holds the domain types and their persistence.
//
// The types live in their own file, separate from the SQL, so that the web
// package can be compiled and tested against them without depending on a
// database being open.
package store

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a row does not exist. Handlers turn it into a
// 404, which is also what a caller who is not allowed to see a row gets — an
// existence check is itself information.
var ErrNotFound = errors.New("not found")

// User is someone Cloudflare Access has let in, or someone who has been
// invited to a collection but has never logged in. Both are rows here: an
// invite has to be able to name a person before they first arrive.
type User struct {
	ID        int64
	Email     string
	CreatedAt time.Time
}

// Collection is a to-do list. OwnerID is the authority for owner-only
// actions; the owner additionally has a membership row so that "collections I
// can see" stays a single join rather than a union.
type Collection struct {
	ID        int64
	Name      string
	OwnerID   int64
	CreatedAt time.Time
}

// Item is one entry on a list. CreatorID is recorded and displayed, but it
// does not gate anything: any member may edit any item.
type Item struct {
	ID           int64
	CollectionID int64
	Title        string
	Body         string
	Done         bool
	CreatorID    int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ItemView is an Item with the creator's email resolved. Templates need the
// email rather than the id, and resolving it in SQL avoids the N+1 that doing
// it per row in the handler would cost.
type ItemView struct {
	Item
	CreatorEmail string
}

// CollectionView is a Collection decorated with what the index page shows:
// who owns it and how much of it is done.
type CollectionView struct {
	Collection
	OwnerEmail string
	ItemCount  int
	DoneCount  int
}
