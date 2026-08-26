package web

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// linkSegment is one run of an item's text. Text is always plain text and
// is always escaped by the template; URL is empty for a segment that is
// not a link.
type linkSegment struct {
	Text string
	URL  string
}

// linkify splits s into a run of plain-text and link segments, recognising
// http://, https:// and bare www. addresses case-insensitively. A www.
// match gets an "https://" URL, but Text stays exactly as typed, so the
// displayed text matches what the person pasted.
//
// It never builds HTML: URL is a bare address for the template's href
// attribute to escape on its own terms, and Text goes through the same
// escaping every other piece of page text does. Building markup here
// instead would make this the one place in the app that assembles it from
// text someone typed, moving a missed escape from a rendering bug into an
// XSS hole.
//
// Known false negative: url.Parse returns an EscapeError for a "%" not
// followed by two hex digits, so a URL like https://shop.example.com/50%off
// is emitted as plain text rather than a link. That is safe — html/template
// would have percent-encoded the "%" anyway — and rare enough not to be
// worth a bespoke pre-scan ahead of url.Parse.
func linkify(s string) []linkSegment {
	var segs []linkSegment
	var plain strings.Builder
	pos := 0
	for pos < len(s) {
		idx, plen := findCandidate(s, pos)
		if idx < 0 {
			plain.WriteString(s[pos:])
			pos = len(s)
			break
		}
		plain.WriteString(s[pos:idx])

		end := idx + tokenEnd(s[idx:])
		token := s[idx:end]
		trimmed, ok := validate(token, plen)
		if ok {
			if plain.Len() > 0 {
				segs = append(segs, linkSegment{Text: plain.String()})
				plain.Reset()
			}
			href := trimmed
			if plen == len("www.") {
				href = "https://" + trimmed
			}
			segs = append(segs, linkSegment{Text: trimmed, URL: href})
			// Whatever validate trimmed off the end (trailing punctuation,
			// an unbalanced closer) is not part of the link — it goes back
			// into the plain run that follows.
			plain.WriteString(token[len(trimmed):])
		} else {
			// Not a link, just prose that happens to start with a matched
			// prefix. The whole token — not just the prefix — becomes
			// plain text, so the scanner does not re-examine anything
			// inside it (see findCandidate's comment on why that matters
			// for a comma-joined paste).
			plain.WriteString(token)
		}
		pos = end
	}
	if plain.Len() > 0 {
		segs = append(segs, linkSegment{Text: plain.String()})
	}
	return segs
}

// findCandidate scans s from byte offset "from" for the next
// case-insensitive occurrence of "http://", "https://" or "www.", and
// returns its offset and the length of whichever prefix matched, or -1 if
// none remain.
//
// The boundary check is a blocklist, not an allowlist: a match is rejected
// only when the rune immediately before it is a letter, digit, ".", "-",
// "_" or "@" — the set that would glue the prefix onto an existing word
// ("seehttp://x", "notwww.example.com"). Requiring a specific opening
// character instead — whitespace, or an opening bracket — would also
// reject "see:https://x", an em dash right before a URL, or a curly quote
// before one, none of which are actually attached to anything.
func findCandidate(s string, from int) (idx, plen int) {
	for i := from; i < len(s); i++ {
		var n int
		switch {
		case hasPrefixFold(s[i:], "https://"):
			n = len("https://")
		case hasPrefixFold(s[i:], "http://"):
			n = len("http://")
		case hasPrefixFold(s[i:], "www."):
			n = len("www.")
		default:
			continue
		}
		if boundaryOK(s, i) {
			return i, n
		}
	}
	return -1, 0
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// boundaryOK reports whether the rune immediately before byte offset i in s
// blocks a match there. See findCandidate's comment for why this is a
// blocklist rather than a requirement for a specific character.
func boundaryOK(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	switch r {
	case '.', '-', '_', '@':
		return false
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// tokenEnd returns how many bytes at the start of s belong to one
// candidate: everything up to the first whitespace rune, or all of s if
// there is none.
//
// unicode.IsSpace, not an ASCII set: a <textarea> submits CRLF, and field
// (routes.go) only trims the outer edges of a value, so a "\r" genuinely
// turns up mid-body, and a pasted NBSP is a realistic artifact too. Either
// would otherwise be swallowed into an href by an ASCII-only check.
func tokenEnd(s string) int {
	for i, r := range s {
		if unicode.IsSpace(r) {
			return i
		}
	}
	return len(s)
}

// trimPunct is the sentence-punctuation trimmed from a candidate's trailing
// edge — characters that close a clause and are essentially never meant to
// be part of the URL a person is pointing at.
const trimPunct = ".,;:!?'\""

// closerOf reports the opening rune paired with closing rune r, for the
// unbalanced-closer rule in trimTrailing below.
//
// ">" pairing with "<" is not in the plan's original three-pair list
// (")]}" only), but without it "<https://x>" — one of the required
// boundary-blocklist cases — can never come out as a link: the trailing
// ">" would stay glued to the host and fail the hostname charset check in
// validate. The same balanced-count reasoning that is already right for
// parens applies unchanged to angle brackets, so it is added here as a
// fourth pair rather than as separate logic.
func closerOf(r rune) (open rune, ok bool) {
	switch r {
	case ')':
		return '(', true
	case ']':
		return '[', true
	case '}':
		return '{', true
	case '>':
		return '<', true
	}
	return 0, false
}

// trimTrailing strips a candidate's trailing edge in one interleaved pass:
// each iteration tries the punctuation set above and the unbalanced-closer
// rule together, and stops the moment neither fires.
//
// Two separate passes — punctuation first, then closers — get
// "(see https://ex.com/a.)" wrong: the closer would only be considered
// after the punctuation pass had already given up, leaving the sentence's
// full stop inside the href. Interleaving also terminates safely, because
// every iteration removes exactly one rune from token.
func trimTrailing(token string) string {
	for {
		r, size := utf8.DecodeLastRuneInString(token)
		if size == 0 {
			return token
		}
		if strings.ContainsRune(trimPunct, r) {
			token = token[:len(token)-size]
			continue
		}
		if open, ok := closerOf(r); ok {
			opens := strings.Count(token, string(open))
			closes := strings.Count(token, string(r))
			if closes > opens {
				token = token[:len(token)-size]
				continue
			}
		}
		return token
	}
}

// wwwHasLabel reports whether rest — the text immediately after a matched
// "www." prefix, host part only — has a further label, e.g. "example.com"
// in "www.example.com/path". Without this check, "check the www. site"
// trims down to "www." plus an empty rest, and url.Parse accepts the
// single-label host "www" without complaint.
func wwwHasLabel(rest string) bool {
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	dot := strings.IndexByte(host, '.')
	return dot > 0 && dot < len(host)-1
}

// validate trims token's trailing edge and checks what remains against the
// rules that separate a real link from prose that merely starts with one
// of the three matched prefixes. A candidate that fails is not an error,
// it is just prose, so the caller falls back to plain text on any failure
// here.
func validate(token string, plen int) (trimmed string, ok bool) {
	trimmed = trimTrailing(token)

	// trimTrailing only ever removes from the end, so trimmed is always a
	// prefix of token: if enough of it survives to cover the matched
	// prefix, that prefix is untouched by construction. This length check
	// is what catches "www." trimming down to "www" — too short to still
	// hold the four characters that were actually matched.
	if len(trimmed) < plen {
		return "", false
	}

	isWWW := plen == len("www.")
	raw := trimmed
	if isWWW {
		if !wwwHasLabel(trimmed[plen:]) {
			return "", false
		}
		raw = "https://" + trimmed
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	// "https://google.com@evil.com/login" passes every other check with
	// Hostname() == "evil.com", while the displayed text — the URL
	// verbatim — reads "google.com". Rejecting any userinfo closes that.
	if u.User != nil {
		return "", false
	}
	// Hostname, not Host: Host accepts a bare port, so "https://:8080/x"
	// has a non-empty Host but an empty Hostname, and would otherwise slip
	// through as a link to nowhere.
	host := u.Hostname()
	if host == "" || !validHostChars(u.Host, host) {
		return "", false
	}

	return trimmed, true
}

// validHostChars applies a conservative charset to a parsed host: ASCII
// letters, digits, ".", "-", "_", any non-ASCII rune (an internationalised
// domain typed as-is rather than in punycode), or a bracketed IPv6
// literal. rawHost is u.Host, which still carries its brackets and any
// port — Hostname has already stripped both, so it is the only place left
// to see them.
//
// This single check is what rejects the common comma-joined paste
// "https://a.com,https://b.com": url.Parse happily accepts its authority
// as a whole, and every browser then refuses to load it, producing a link
// that does nothing when clicked.
func validHostChars(rawHost, host string) bool {
	if strings.HasPrefix(rawHost, "[") {
		return true
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		case r > unicode.MaxASCII:
		default:
			return false
		}
	}
	return true
}
