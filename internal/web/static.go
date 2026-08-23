package web

import (
	"net/http"

	"list/internal/assets"
)

// staticHandler serves the vendored front-end files. It lives behind a thin
// wrapper rather than being referenced directly in Handler so that the asset
// package stays the only thing that knows how they are embedded.
func staticHandler() http.Handler {
	return assets.Handler()
}
