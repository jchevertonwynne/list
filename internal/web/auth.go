package web

import (
	"log"
	"net/http"
	"strings"
)

// accessEmailHeader is set by Cloudflare Access once it has authenticated the
// request at the edge. The origin Service is ClusterIP, reachable only from
// the cloudflared pod, so trusting this header without also verifying
// Cf-Access-Jwt-Assertion is a deliberate trade-off: nothing but that one pod
// can reach the origin to forge it, and doing JWT verification here would
// just be checking a lock on a door only one key exists for.
const accessEmailHeader = "Cf-Access-Authenticated-User-Email"

// authenticate resolves an identity for every request behind it, or rejects
// the request outright. Every downstream handler assumes userFrom succeeds;
// this is the only place that assumption gets made true.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every authenticated page is specific to the user who requested it,
		// so it must never be served from a shared cache — Cloudflare's edge
		// cache in particular, which sits in front of this exact header.
		w.Header().Set("Cache-Control", "no-store")

		email := normalizeEmail(r.Header.Get(accessEmailHeader))
		if email == "" {
			// devUser stands in for the Access header when running locally,
			// where there is no Access in front of anything. In production
			// it is empty, so this is a no-op there.
			email = normalizeEmail(s.devUser)
		}
		if email == "" {
			// This is the one rejection that matters most in this file. If
			// the Access application in front of this hostname were ever
			// misconfigured, removed, or bypassed, the header would simply
			// stop arriving — and falling through to an empty-string
			// identity would silently merge every visitor into one shared
			// anonymous account, with everyone seeing everyone else's
			// lists. Refuse to guess an identity instead.
			log.Printf("authenticate: no %s header and no devUser configured", accessEmailHeader)
			http.Error(w, "not authenticated", http.StatusForbidden)
			return
		}

		// UserByEmail upserts, so a first-time Access login and a
		// previously-invited-but-never-seen email both resolve here without
		// a separate signup step.
		u, err := s.store.UserByEmail(r.Context(), email)
		if err != nil {
			// The error may hold details about the store (a DSN, a query) that
			// have no business reaching a client.
			log.Printf("authenticate: UserByEmail(%s): %v", email, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
