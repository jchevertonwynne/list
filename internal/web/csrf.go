package web

import (
	"log"
	"net/http"
	"net/url"
	"strings"
)

// guardCSRF protects state-changing requests.
//
// Authentication here rides on a cookie — Cloudflare Access's CF_Authorization
// — which the browser attaches to any request to this origin, including one
// triggered by a form on someone else's site. "Authenticated" is therefore not
// the same as "intended", and Access does nothing about the difference.
//
// The defence is to require a header that a cross-origin caller cannot set.
// HX-Request is not a CORS-safelisted header, so a cross-origin fetch that
// tries to send it must first pass a preflight, and this server answers no
// preflight at all. A plain HTML form — the classic CSRF vector — cannot set
// custom headers under any circumstances. So the header's mere presence is
// the proof, and no token needs storing anywhere, which suits an app that
// keeps no session state of its own.
//
// The Origin check is belt and braces for the case where a future browser
// relaxes something above.
func guardCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		if r.Header.Get("HX-Request") != "true" {
			log.Printf("guardCSRF: %s %s without HX-Request", r.Method, r.URL.Path)
			http.Error(w, "request must come from the application", http.StatusForbidden)
			return
		}

		// Origin is absent on some same-origin requests, which is why its
		// absence cannot be treated as failure; present and mismatched is a
		// different matter.
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				log.Printf("guardCSRF: cross-origin %s %s from %q", r.Method, r.URL.Path, origin)
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
