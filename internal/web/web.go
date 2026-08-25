// Package web is the HTTP surface: routing, authorisation and rendering.
//
// It depends on the store through an interface rather than a concrete type so
// that handlers can be tested against a fake without opening a database.
package web

import (
	"context"
	"net/http"

	"list/internal/live"
	"list/internal/metrics"
	"list/internal/store"
	"list/internal/tracing"
)

// Store is everything the HTTP layer needs from persistence.
type Store interface {
	// UserByEmail upserts: an email that has never been seen becomes a user.
	// Both authentication and invitation rely on that.
	UserByEmail(ctx context.Context, email string) (store.User, error)

	CollectionsForUser(ctx context.Context, userID int64) ([]store.CollectionView, error)
	CreateCollection(ctx context.Context, ownerID int64, name string) (store.Collection, error)
	Collection(ctx context.Context, id int64) (store.Collection, error)
	IsMember(ctx context.Context, collectionID, userID int64) (bool, error)
	RenameCollection(ctx context.Context, id int64, name string) error
	DeleteCollection(ctx context.Context, id int64) error

	Items(ctx context.Context, collectionID int64) ([]store.ItemView, error)
	CreateItem(ctx context.Context, collectionID, creatorID int64, title, body string) (store.ItemView, error)
	Item(ctx context.Context, id int64) (store.ItemView, error)
	UpdateItem(ctx context.Context, id int64, title, body string) (store.ItemView, error)
	ToggleItem(ctx context.Context, id int64) (store.ItemView, error)
	DeleteItem(ctx context.Context, id int64) error

	Members(ctx context.Context, collectionID int64) ([]store.User, error)
	AddMember(ctx context.Context, collectionID int64, email string) (store.User, error)
	RemoveMember(ctx context.Context, collectionID, userID int64) error
}

// Server holds the dependencies every handler needs.
type Server struct {
	store Store
	// devUser stands in for the Access header when running locally, where
	// there is no Access in front of anything. Empty in production, and an
	// empty devUser with no header is a rejected request, never an anonymous
	// one.
	devUser string
	live    *live.Hub
}

func New(s Store, devUser string) *Server {
	return &Server{store: s, devUser: devUser, live: live.New()}
}

// Close shuts down everything that outlives a single request. It must run
// before http.Server.Shutdown — see main.go for why: unlike a hijacked
// connection, an SSE handler is an ordinary one Shutdown waits for, so
// closing the hub first is what makes those handlers return promptly instead
// of holding the shutdown timeout open.
func (s *Server) Close() {
	s.live.Close()
}

// Handler builds the mux. Everything except /healthz, /static/ and /metrics
// sits behind authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: the probes must not depend on Access, the assets are
	// the same for everyone, and Alloy scraping /metrics has no Access
	// session to present.
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("GET /static/", staticHandler())
	mux.Handle("GET /metrics", metrics.Handler())

	// Authenticated application routes are registered on their own mux so a
	// single wrapper covers all of them — a route added later cannot forget
	// to authenticate.
	app := http.NewServeMux()
	s.routes(app)

	// authenticate outermost so that every response — including one the CSRF
	// guard rejects — carries Cache-Control: no-store, and so that the guard's
	// log lines can name the user they came from.
	mux.Handle("/", s.authenticate(guardCSRF(app)))

	// Instrument wraps the whole mux, not just app: /healthz and /metrics
	// scrapes are frequent enough that leaving them out would undercount
	// request volume relative to what's actually hitting the pod. app is
	// passed alongside it purely so Instrument can find the real matched
	// pattern for authenticated routes — mux itself only sees them behind
	// the "/" catch-all it hands off to s.authenticate(guardCSRF(app)).
	// tracing wraps outermost so the span covers metrics recording and
	// auth too.
	return tracing.Middleware("list", metrics.Instrument(mux, app))
}

// handleHealthz is what the readiness and liveness probes hit. Cheap and
// dependency-free on purpose: it runs several times a minute forever, and a
// broken database should surface as a 500 on a real request rather than as a
// restart loop.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}
