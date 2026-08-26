package web

import (
	"bytes"
	"embed"
	"html/template"
	"log"
	"net/http"

	"list/internal/store"
)

//go:embed templates
var templateFS embed.FS

// pageData is what every template receives. One struct for every page rather
// than one per page: the layout needs UserEmail and Title from all of them,
// and a handful of unused nil fields costs less than an interface plus a type
// switch in the layout.
type pageData struct {
	UserEmail string
	Title     string

	Collections []store.CollectionView
	Collection  store.Collection
	Items       []store.ItemView
	Members     []store.User
	IsOwner     bool
	Error       string

	// CoverETag, CoverWidth and CoverHeight describe a collection's cover for
	// the banner <img> on collection.html. They live here rather than on
	// store.Collection for the same reason itemCtx exists above: a rendering
	// concern — what the template needs to draw a banner without shifting the
	// page — does not belong on a store type. An empty CoverETag means "no
	// cover", the ordinary case for most collections, not an error; see
	// store.CollectionImageETag.
	CoverETag   string
	CoverWidth  int
	CoverHeight int
}

// pages holds one fully-parsed template set per page.
//
// Each page gets its OWN set because every page defines a template called
// "content" — parsing them all into one set would leave whichever was parsed
// last as the definition for every page, which fails silently by rendering
// the wrong body rather than by erroring.
var pages = map[string]*template.Template{}

// fragments is the set used for htmx responses that are not whole pages. The
// same definitions are also parsed into every page set, so a row rendered
// standalone after an edit is byte-identical to the one rendered as part of
// the full page. Two copies of that markup would drift.
var fragments *template.Template

// funcs is the template function set.
//
// itemCtx exists because html/template can only pass a single value to a
// nested template, and an item row needs both the item and the collection it
// belongs to in order to build its own action URLs. The alternative — storing
// the collection id on every ItemView — would put a rendering concern into
// the store.
//
// linkify is here for the same kind of reason: every render path — a full
// page, an htmx swap, a live re-fetch after an SSE nudge — ends up calling
// "item" in fragments.html, so a template function reaches all of them for
// free. Splitting a title or body into segments in the handler instead would
// mean every one of those callers had to build the segments itself before it
// could reach the template at all, for a value the template needs and
// nothing else does. No second registration was needed to get it there: this
// map is already passed to .Funcs on both the per-page sets and the
// standalone fragments set below, so one entry here covers every one of
// those render paths.
var funcs = template.FuncMap{
	"itemCtx": func(item store.ItemView, c store.Collection) itemCtx {
		return itemCtx{Item: item, Collection: c}
	},
	"linkify": linkify,
}

func init() {
	const layout = "templates/layout.html"
	const frags = "templates/fragments.html"

	for _, page := range []string{"index.html", "collection.html"} {
		pages[page] = template.Must(
			template.New(page).Funcs(funcs).ParseFS(templateFS, layout, frags, "templates/"+page))
	}
	fragments = template.Must(
		template.New("fragments").Funcs(funcs).ParseFS(templateFS, frags))
}

// render writes a full page. It renders into a buffer first so that a
// template error becomes a clean 500 rather than a 200 with half a page
// already on the wire, which is impossible to diagnose from the client end.
func (s *Server) render(w http.ResponseWriter, page string, data pageData) {
	t, ok := pages[page]
	if !ok {
		log.Printf("render: no such page %q", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// renderFragment writes one named block, for an htmx swap.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := fragments.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("renderFragment %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
