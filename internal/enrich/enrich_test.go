package enrich

import "testing"

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		// Query strings are the single biggest source of accidental cardinality.
		{"https://x.com/about?utm_source=hn&id=99", "/about"},
		{"https://x.com/about#section", "/about"},
		{"https://x.com/about/", "/about"},
		{"https://x.com/", "/"},
		{"https://x.com", "/"},
		{"/relative/page", "/relative/page"},
		{"https://x.com/About/US", "/about/us"},

		// Identifier-shaped segments collapse.
		{"https://x.com/users/550e8400-e29b-41d4-a716-446655440000", "/users/:id"},
		{"https://x.com/posts/123456", "/posts/:id"},
		{"https://x.com/o/deadbeefcafe0123", "/o/:id"},

		// Real routes that merely look numeric must survive.
		{"https://x.com/2024/review", "/2024/review"},
		{"https://x.com/page/2", "/page/2"},
		{"https://x.com/blog/my-post-title", "/blog/my-post-title"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePathIsBounded(t *testing.T) {
	long := "/"
	for i := 0; i < 500; i++ {
		long += "abcdefgh"
	}
	if got := NormalizePath("https://x.com" + long); len(got) > MaxPathLen {
		t.Errorf("path length %d exceeds cap %d", len(got), MaxPathLen)
	}
}

func TestReferrerHost(t *testing.T) {
	cases := []struct{ raw, self, want string }{
		{"https://news.ycombinator.com/item?id=1", "x.com", "news.ycombinator.com"},
		{"https://www.google.com/search?q=secret", "x.com", "google.com"},
		// Internal navigation is not a referrer.
		{"https://x.com/other", "x.com", ""},
		{"https://www.x.com/other", "x.com", ""},
		{"", "x.com", ""},
		{"not a url", "x.com", ""},
	}
	for _, c := range cases {
		if got := ReferrerHost(c.raw, c.self); got != c.want {
			t.Errorf("ReferrerHost(%q, %q) = %q, want %q", c.raw, c.self, got, c.want)
		}
	}
}

// TestReferrerDropsPath guards a privacy property: the referrer path can carry
// somebody else's private URL, so only the host may survive.
func TestReferrerDropsPath(t *testing.T) {
	got := ReferrerHost("https://mail.example.com/inbox/secret-thread-id", "x.com")
	if got != "mail.example.com" {
		t.Errorf("got %q, want bare host with no path", got)
	}
}

func TestParseUTM(t *testing.T) {
	u := ParseUTM("https://x.com/p?utm_source=HN&utm_medium=social&utm_campaign=Launch")
	if u.Source != "hn" || u.Medium != "social" || u.Campaign != "launch" {
		t.Errorf("got %+v, want lowercased hn/social/launch", u)
	}
	if got := ParseUTM("https://x.com/p?ref=twitter").Source; got != "twitter" {
		t.Errorf("ref alias = %q, want twitter", got)
	}
	if got := ParseUTM("https://x.com/p").Source; got != "" {
		t.Errorf("absent utm = %q, want empty", got)
	}
}

func TestParseUA(t *testing.T) {
	cases := []struct {
		ua                  string
		browser, os, device string
	}{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
			"Chrome", "macOS", "desktop"},
		// Edge claims to be both Chrome and Safari; order of rules decides.
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36 Edg/120.0",
			"Edge", "Windows", "desktop"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
			"Safari", "macOS", "desktop"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"Safari", "iOS", "mobile"},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"Safari", "iOS", "tablet"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
			"Firefox", "Linux", "desktop"},
		// Android tablets omit the "mobile" token that phones carry.
		{"Mozilla/5.0 (Linux; Android 13; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36",
			"Chrome", "Android", "mobile"},
		{"Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
			"Chrome", "Android", "tablet"},
	}
	for _, c := range cases {
		got := ParseUA(c.ua)
		if got.Bot {
			t.Errorf("%.40s… flagged as bot", c.ua)
			continue
		}
		if got.Browser != c.browser || got.OS != c.os || got.Device != c.device {
			t.Errorf("ParseUA(%.40s…) = %s/%s/%s, want %s/%s/%s",
				c.ua, got.Browser, got.OS, got.Device, c.browser, c.os, c.device)
		}
	}
}

func TestParseUADetectsBots(t *testing.T) {
	bots := []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"curl/8.4.0",
		"python-requests/2.31.0",
		"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/120.0",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"UptimeRobot/2.0",
	}
	for _, ua := range bots {
		if !ParseUA(ua).Bot {
			t.Errorf("%q not flagged as bot", ua)
		}
	}
}

func TestScreenClass(t *testing.T) {
	cases := []struct {
		w    int
		want string
	}{{0, ""}, {375, "xs"}, {700, "sm"}, {800, "md"}, {1200, "lg"}, {1920, "xl"}}
	for _, c := range cases {
		if got := ScreenClass(c.w); got != c.want {
			t.Errorf("ScreenClass(%d) = %q, want %q", c.w, got, c.want)
		}
	}
}
