package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestCounterJSIsEmbedded(t *testing.T) {
	if len(CounterJS) == 0 {
		t.Fatal("counter.js was not embedded")
	}
	if !strings.Contains(string(CounterJS), "/api/event") {
		t.Error("embedded script does not reference the ingest endpoint")
	}
}

// TestNoHardcodedEndpoint is the important one. data-host is mandatory and
// there must be no fallback URL anywhere in the source: a fork that forgets to
// set it should be a silent no-op, never a script that quietly reports to
// somebody else's server.
func TestNoHardcodedEndpoint(t *testing.T) {
	src := string(CounterJS)

	// Any absolute URL outside a comment is suspect.
	url := regexp.MustCompile(`https?://[^\s'"]+`)
	for _, line := range executableLines(src) {
		if m := url.FindString(line); m != "" {
			t.Errorf("hardcoded URL %q in executable line: %s", m, strings.TrimSpace(line))
		}
	}

	if !strings.Contains(src, "data-host") {
		t.Error("script should read its endpoint from data-host")
	}
}

// TestNoPersistentStorage guards the privacy claim at the source level: the
// script must never touch cookies or web storage.
func TestNoPersistentStorage(t *testing.T) {
	// Scan executable lines only — the header comment states these very words
	// as a promise, and matching it would be a false positive.
	for i, line := range executableLines(string(CounterJS)) {
		for _, banned := range []string{
			"document.cookie", "localStorage", "sessionStorage", "indexedDB",
		} {
			if strings.Contains(line, banned) {
				t.Errorf("line %d uses %s; the no-persistent-identifier claim depends on it not doing that\n\t%s",
					i+1, banned, line)
			}
		}
	}
}

// executableLines drops comment-only lines so source assertions do not trip
// over documentation that describes what the code deliberately avoids.
func executableLines(src string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case inBlock:
			if strings.Contains(t, "*/") {
				inBlock = false
			}
		case strings.HasPrefix(t, "/*"):
			if !strings.Contains(t, "*/") {
				inBlock = true
			}
		case strings.HasPrefix(t, "//"), t == "":
			// skip
		default:
			out = append(out, line)
		}
	}
	return out
}

func TestScriptStaysSmall(t *testing.T) {
	// Unminified. The minified+gzipped artifact is far smaller, but a large
	// jump here still means something has crept in.
	const limit = 8 << 10
	if len(CounterJS) > limit {
		t.Errorf("counter.js is %d bytes, over the %d-byte source budget", len(CounterJS), limit)
	}
}
