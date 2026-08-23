// Package assets embeds the vendored front-end files — Beer CSS, htmx and the
// Material Symbols icon font — into the binary.
//
// The deployment target is a container FROM scratch with
// readOnlyRootFilesystem: true, so there is no filesystem to read these files
// from at runtime even if we wanted one. go:embed is not a convenience here;
// it is the only way these bytes reach the running process at all.
package assets

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
)

//go:embed static
var files embed.FS

func init() {
	// mime.TypeByExtension consults the OS's mime.types on some platforms,
	// and ".woff2" isn't guaranteed to be in it — the Pi's minimal base image
	// is exactly the kind of place it might be missing. Registering it
	// ourselves means the content type is correct everywhere this binary
	// runs, not just on whatever machine happens to have font mimes
	// configured.
	mime.AddExtensionType(".woff2", "font/woff2")
}

// Handler serves the embedded files. It is built to be mounted at
// "GET /static/": the caller strips nothing itself, so the prefix stripping
// happens in here.
//
// fs.Sub rebases the embedded tree so "static/beer.min.css" is served as
// "beer.min.css" — i.e. the URL directory is "/static/" for every file, with
// none of them nested under an extra "static/" segment. That matters beyond
// tidiness: Beer CSS's @font-face references the icon font by a path
// relative to beer.min.css, so the two files must resolve as siblings under
// the same URL directory or icons render as literal text like
// "check_box" instead of a glyph.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		// Only fails if the embed directive above no longer matches an
		// actual "static" directory, which is a build-time mistake, not a
		// condition to handle gracefully at runtime.
		panic(err)
	}

	fileServer := http.FileServerFS(sub)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every asset here is baked into this image at build time. A pod
		// only ever runs one image, so the only way an asset's bytes change
		// is a new image, which is a new pod at a new URL cache generation —
		// there is no scenario where a client would need to re-fetch a path
		// it already has. That makes the strongest cache directive available
		// safe, which a general-purpose static file server could never
		// assume on its own.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})

	return http.StripPrefix("/static/", handler)
}
