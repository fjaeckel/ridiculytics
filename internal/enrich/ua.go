package enrich

import "strings"

// Client is the parsed User-Agent, reduced to bounded values.
type Client struct {
	Browser string
	OS      string
	Device  string // desktop | mobile | tablet
	Bot     bool
}

// botMarkers are substrings that identify automated traffic. Matching is
// deliberately broad: a false positive costs one uncounted pageview, a false
// negative pollutes every metric a real user appears in.
var botMarkers = []string{
	"bot", "crawler", "spider", "crawling", "slurp", "curl/", "wget/",
	"python-requests", "python-urllib", "go-http-client", "java/", "okhttp",
	"headlesschrome", "phantomjs", "puppeteer", "playwright", "selenium",
	"lighthouse", "pagespeed", "gtmetrix", "pingdom", "uptimerobot",
	"monitoring", "http_request", "libwww-perl", "scrapy", "axios/",
	"node-fetch", "postmanruntime", "insomnia", "facebookexternalhit",
	"preview", "feedfetcher", "apache-httpclient", "dataprovider",
	"probe", "checker", "validator", "archiver", "wappalyzer",
}

// browserRules are evaluated in order; the first match wins. Order matters
// enormously here because nearly every browser lies about being the others:
// Edge claims Chrome and Safari, Chrome claims Safari, and so on. The most
// specific impostors must be tested first.
var browserRules = []struct{ marker, name string }{
	{"edg/", "Edge"},
	{"edga/", "Edge"},
	{"edgios/", "Edge"},
	{"opr/", "Opera"},
	{"opera", "Opera"},
	{"vivaldi", "Vivaldi"},
	{"brave", "Brave"},
	{"yabrowser", "Yandex"},
	{"samsungbrowser", "Samsung Internet"},
	{"ucbrowser", "UC Browser"},
	{"duckduckgo", "DuckDuckGo"},
	{"firefox/", "Firefox"},
	{"fxios/", "Firefox"},
	{"chromium", "Chromium"},
	{"crios/", "Chrome"},
	{"chrome/", "Chrome"},
	{"safari/", "Safari"},
	{"msie ", "Internet Explorer"},
	{"trident/", "Internet Explorer"},
}

var osRules = []struct{ marker, name string }{
	{"windows nt 10", "Windows"},
	{"windows nt", "Windows"},
	{"windows phone", "Windows Phone"},
	{"android", "Android"},
	{"cros ", "ChromeOS"},
	{"iphone", "iOS"},
	{"ipad", "iOS"},
	{"ipod", "iOS"},
	{"mac os x", "macOS"},
	{"macintosh", "macOS"},
	{"ubuntu", "Linux"},
	{"fedora", "Linux"},
	{"debian", "Linux"},
	{"linux", "Linux"},
	{"freebsd", "BSD"},
	{"openbsd", "BSD"},
}

// ParseUA extracts browser, OS and device class from a User-Agent string.
//
// This is intentionally hand-rolled rather than pulled from a library: UA
// databases are large, frequently updated, and a supply-chain dependency for
// a project promising to work unchanged for years. The output is only ever a
// handful of bounded label values, so approximate parsing is acceptable.
func ParseUA(ua string) Client {
	if ua == "" {
		return Client{Browser: "Unknown", OS: "Unknown", Device: "desktop"}
	}
	l := strings.ToLower(ua)

	for _, m := range botMarkers {
		if strings.Contains(l, m) {
			return Client{Bot: true}
		}
	}

	c := Client{Browser: "Other", OS: "Other"}
	for _, r := range browserRules {
		if strings.Contains(l, r.marker) {
			c.Browser = r.name
			break
		}
	}
	for _, r := range osRules {
		if strings.Contains(l, r.marker) {
			c.OS = r.name
			break
		}
	}
	c.Device = deviceClass(l)
	return c
}

// deviceClass distinguishes tablet from mobile from desktop. Android is the
// awkward case: Android tablets omit the "mobile" token that Android phones
// carry, so absence of "mobile" is the tablet signal.
func deviceClass(l string) string {
	switch {
	case strings.Contains(l, "ipad"),
		strings.Contains(l, "tablet"),
		strings.Contains(l, "kindle"),
		strings.Contains(l, "playbook"),
		strings.Contains(l, "silk"):
		return "tablet"
	case strings.Contains(l, "android") && !strings.Contains(l, "mobile"):
		return "tablet"
	case strings.Contains(l, "mobi"),
		strings.Contains(l, "iphone"),
		strings.Contains(l, "ipod"),
		strings.Contains(l, "windows phone"),
		strings.Contains(l, "android"):
		return "mobile"
	default:
		return "desktop"
	}
}
