// Package enrich turns a raw event into bounded, label-safe dimensions.
package enrich

import (
	"net/url"
	"strconv"
	"strings"
)

// MaxPathLen bounds a path before it can become a label value.
const MaxPathLen = 128

// NormalizePath extracts a stable, low-cardinality path from a page URL.
// Query strings and fragments are dropped entirely (they are the single
// biggest source of accidental cardinality), and identifier-shaped segments
// collapse to :id so /users/8f3a... and /users/91bc... share one series.
func NormalizePath(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "/"
	}
	p := u.Path
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.ToLower(p)

	// Trailing slash is noise: /about and /about/ are one page.
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}

	segs := strings.Split(p, "/")
	for i, s := range segs {
		if isIdentifier(s) {
			segs[i] = ":id"
		}
	}
	p = strings.Join(segs, "/")

	if len(p) > MaxPathLen {
		p = p[:MaxPathLen]
	}
	return p
}

// isIdentifier matches segments that are almost certainly opaque ids: UUIDs,
// long hex, and pure digit runs. Short numbers are left alone because they are
// often real routes (/2024, /page/2) rather than identifiers.
func isIdentifier(s string) bool {
	if len(s) < 2 {
		return false
	}
	if isUUID(s) {
		return true
	}

	digits, hexish, other := 0, 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
			hexish++
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			hexish++
		case r == '-' || r == '_':
			// separator, ignored
		default:
			other++
		}
	}
	if other > 0 {
		return false
	}
	if digits == len(s) && len(s) >= 4 {
		// Years are the common false positive: /2024/review and /blog/2019
		// are real routes, not object ids. Four-digit values in a sensible
		// year range are left alone; everything longer is an id.
		if len(s) == 4 {
			y, err := strconv.Atoi(s)
			if err == nil && y >= 1900 && y <= 2100 {
				return false
			}
		}
		return true // numeric id
	}
	if hexish == len(s) && len(s) >= 12 {
		return true // hash / object id
	}
	return false
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// UTM holds campaign attribution pulled from the page URL.
type UTM struct {
	Source   string
	Medium   string
	Campaign string
}

// MaxTagLen bounds any single campaign tag.
const MaxTagLen = 64

// ParseUTM reads utm_* parameters, accepting the common short aliases that
// most collectors also honour.
func ParseUTM(rawURL string) UTM {
	u, err := url.Parse(rawURL)
	if err != nil {
		return UTM{}
	}
	q := u.Query()
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				return clamp(strings.ToLower(v), MaxTagLen)
			}
		}
		return ""
	}
	return UTM{
		Source:   pick("utm_source", "source", "ref"),
		Medium:   pick("utm_medium", "medium"),
		Campaign: pick("utm_campaign", "campaign"),
	}
}

// ReferrerHost reduces a referrer to its bare host. The full referrer URL is
// deliberately discarded: it is both a cardinality bomb and the one field most
// likely to carry someone else's private path.
func ReferrerHost(raw, selfDomain string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	h = strings.TrimPrefix(h, "www.")
	self := strings.TrimPrefix(strings.ToLower(selfDomain), "www.")
	if h == self {
		return "" // internal navigation is not a referrer
	}
	return clamp(h, MaxTagLen)
}

// ScreenClass buckets viewport width into a handful of classes, so screen
// size costs 5 series instead of one per pixel width.
func ScreenClass(w int) string {
	switch {
	case w <= 0:
		return ""
	case w < 576:
		return "xs"
	case w < 768:
		return "sm"
	case w < 992:
		return "md"
	case w < 1440:
		return "lg"
	default:
		return "xl"
	}
}

func clamp(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
