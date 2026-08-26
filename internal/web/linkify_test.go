package web

import (
	"reflect"
	"strings"
	"testing"
)

// link builds the common case, where the displayed text and the href are
// identical, so the table below does not have to repeat every URL twice.
func link(s string) linkSegment { return linkSegment{Text: s, URL: s} }

func text(s string) linkSegment { return linkSegment{Text: s} }

var linkifyCases = []struct {
	name string
	in   string
	want []linkSegment
}{
	{
		name: "plain text with no URL",
		in:   "just some plain text",
		want: []linkSegment{text("just some plain text")},
	},
	{
		name: "URL alone",
		in:   "https://example.com",
		want: []linkSegment{link("https://example.com")},
	},
	{
		name: "URL at start of a sentence",
		in:   "https://example.com is the site",
		want: []linkSegment{link("https://example.com"), text(" is the site")},
	},
	{
		name: "URL in the middle of a sentence",
		in:   "visit https://example.com today",
		want: []linkSegment{text("visit "), link("https://example.com"), text(" today")},
	},
	{
		name: "URL at the end of a sentence",
		in:   "go to https://example.com",
		want: []linkSegment{text("go to "), link("https://example.com")},
	},
	{
		name: "two adjacent URLs separated by one space",
		in:   "https://a.com https://b.com",
		want: []linkSegment{link("https://a.com"), text(" "), link("https://b.com")},
	},
	{
		name: "a trailing punctuation run",
		in:   "see https://x.com... now",
		want: []linkSegment{text("see "), link("https://x.com"), text("... now")},
	},
	{
		name: "the interleaved-trim case: (see https://ex.com/a.)",
		in:   "(see https://ex.com/a.)",
		want: []linkSegment{text("(see "), link("https://ex.com/a"), text(".)")},
	},
	{
		name: "balanced parens are kept",
		in:   "https://en.wikipedia.org/wiki/Foo_(bar)",
		want: []linkSegment{link("https://en.wikipedia.org/wiki/Foo_(bar)")},
	},
	{
		name: "an unbalanced trailing paren is dropped",
		in:   "(see https://example.com)",
		want: []linkSegment{text("(see "), link("https://example.com"), text(")")},
	},
	{
		name: "two closes against one open trims exactly one",
		in:   "https://en.wikipedia.org/wiki/Foo_(bar))",
		want: []linkSegment{link("https://en.wikipedia.org/wiki/Foo_(bar)"), text(")")},
	},
	{
		name: "www.example.com gets an https:// href, text unchanged",
		in:   "www.example.com",
		want: []linkSegment{{Text: "www.example.com", URL: "https://www.example.com"}},
	},
	{
		name: "HTTP://EXAMPLE.COM matches case-insensitively",
		in:   "HTTP://EXAMPLE.COM",
		want: []linkSegment{link("HTTP://EXAMPLE.COM")},
	},
	{
		name: "seehttp://x does not match: the prefix is glued to a word",
		in:   "seehttp://x",
		want: []linkSegment{text("seehttp://x")},
	},
	{
		name: "notwww.example.com does not match either",
		in:   "notwww.example.com",
		want: []linkSegment{text("notwww.example.com")},
	},
	{
		name: "see:https://x matches: the blocklist boundary allows a colon",
		in:   "see:https://x",
		want: []linkSegment{text("see:"), link("https://x")},
	},
	{
		name: "<https://x> matches: the closing bracket is trimmed as unbalanced",
		in:   "<https://x>",
		want: []linkSegment{text("<"), link("https://x"), text(">")},
	},
	{
		name: "check the www. site produces no link",
		in:   "check the www. site",
		want: []linkSegment{text("check the www. site")},
	},
	{
		name: "https://:8080/x is rejected: Host is non-empty but Hostname is not",
		in:   "https://:8080/x",
		want: []linkSegment{text("https://:8080/x")},
	},
	{
		name: "a comma-joined paste is rejected as a single unclickable token",
		in:   "https://a.com,https://b.com",
		want: []linkSegment{text("https://a.com,https://b.com")},
	},
	{
		name: "userinfo is rejected: the link text would lie about the host",
		in:   "https://google.com@evil.com/login",
		want: []linkSegment{text("https://google.com@evil.com/login")},
	},
	{
		name: "javascript: is never even a candidate",
		in:   "javascript:alert(1)",
		want: []linkSegment{text("javascript:alert(1)")},
	},
	{
		name: "file:// is never even a candidate",
		in:   "file:///etc/passwd",
		want: []linkSegment{text("file:///etc/passwd")},
	},
	{
		name: "a URL containing a double quote, angle brackets and an ampersand",
		in:   `https://example.com/search?q="a"&x=<b>`,
		want: []linkSegment{link(`https://example.com/search?q="a"&x=<b>`)},
	},
	{
		name: `\r terminates a token`,
		in:   "https://a.com\rhttps://b.com",
		want: []linkSegment{link("https://a.com"), text("\r"), link("https://b.com")},
	},
	{
		name: "NBSP terminates a token",
		in:   "https://a.com https://b.com",
		want: []linkSegment{link("https://a.com"), text(" "), link("https://b.com")},
	},
	{
		name: "a bracketed IPv6 literal is accepted",
		in:   "http://[::1]:8080/x",
		want: []linkSegment{link("http://[::1]:8080/x")},
	},
	{
		name: "empty string",
		in:   "",
		want: nil,
	},
	{
		name: "a string that is only whitespace",
		in:   "   \t  ",
		want: []linkSegment{text("   \t  ")},
	},
}

func TestLinkify(t *testing.T) {
	for _, tc := range linkifyCases {
		t.Run(tc.name, func(t *testing.T) {
			got := linkify(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("linkify(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLinkifyReconstructsInput is the property that matters most: whatever
// the trimming and validation logic decides about any given rune, it must
// never drop or duplicate one. Concatenating every segment's Text has to
// reproduce the input byte for byte, on the table above and on a handful of
// adversarial strings not otherwise covered.
func TestLinkifyReconstructsInput(t *testing.T) {
	inputs := make([]string, 0, len(linkifyCases))
	for _, tc := range linkifyCases {
		inputs = append(inputs, tc.in)
	}
	inputs = append(inputs,
		"aaa)))+((()) https://example.com)))",
		"\r\n\t   mixed whitespace https://a.b https://c.d",
		"%%%%https://shop.example.com/50%off%%%%",
		"日本語 https://例え.jp テスト",
		">>>><<<< https://x.com<<<<>>>>",
		strings.Repeat("www.a.b ", 50),
	)

	for _, in := range inputs {
		var got strings.Builder
		for _, seg := range linkify(in) {
			got.WriteString(seg.Text)
		}
		if got.String() != in {
			t.Errorf("linkify(%q): segments reconstruct to %q, want the exact input", in, got.String())
		}
	}
}
