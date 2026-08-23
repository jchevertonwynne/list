package web

import (
	"context"
	"net/http"

	"list/internal/store"
)

// ctxKey is unexported so nothing outside this package can put a user into a
// request context. The only way a handler sees an identity is if the
// authentication middleware put it there.
type ctxKey int

const userKey ctxKey = iota

func withUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// userFrom returns the authenticated user. The boolean is false only if the
// middleware did not run, which is a programming error rather than an
// authentication failure — handlers treat it as a 500, never as a guest.
func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

// currentUser is the handler-side helper: it writes the 500 itself so that
// call sites stay a single if-statement.
func currentUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	u, ok := userFrom(r.Context())
	if !ok {
		http.Error(w, "no authenticated user in context", http.StatusInternalServerError)
		return store.User{}, false
	}
	return u, true
}
