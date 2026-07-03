package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// errCrossSite is the rejection a cross-site write earns: a browser form POSTing
// from another origin (the CSRF vector guardCrossSite blocks).
var errCrossSite = errors.New("cross-site request blocked")

// Cross-site guard header names and the one Sec-Fetch-Site value that is
// unambiguously hostile. Named constants keep the header strings out of the
// logic and out of goconst's sights.
const (
	headerSecFetchSite = "Sec-Fetch-Site"
	headerOrigin       = "Origin"
	secFetchCrossSite  = "cross-site"
)

// guardCrossSite rejects browser forms POSTing from another origin — the CSRF
// vector codex flagged on /shutdown, which applies to every write route. It is
// not token-based CSRF: this is a local single-user tool, so a header check is
// the right weight (no cookies, no tokens).
//
// Decision order:
//   - Sec-Fetch-Site (modern browsers send it on every request): allow
//     same-origin / same-site / none, reject cross-site. This is authoritative
//     when present because the browser sets it, not the page.
//   - else Origin (older browsers, or fetch without Sec-Fetch-Site): allow only
//     when its host matches the request Host; a mismatch is cross-site.
//   - neither header: allow. A request with no Origin and no Sec-Fetch-Site is a
//     CLI client (curl) or a same-origin htmx call on a client that omits both —
//     not a cross-site browser form, which is the only thing being blocked.
func (s *Server) guardCrossSite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameSite(r) {
			// renderError maps errCrossSite to 403 (statusForError), so a rejected write
			// gets the same legible error fragment the read errors use.
			s.renderError(w, errCrossSite)
			return
		}
		next(w, r)
	}
}

// sameSite reports whether r is safe to mutate from — same-origin, or no
// cross-site browser signal at all. See guardCrossSite for the decision order.
func sameSite(r *http.Request) bool {
	if site := r.Header.Get(headerSecFetchSite); site != "" {
		return site != secFetchCrossSite
	}
	if origin := r.Header.Get(headerOrigin); origin != "" {
		return originMatchesHost(origin, r.Host)
	}
	return true
}

// originMatchesHost reports whether an Origin URL's host equals the request
// Host. A malformed Origin, or one with no host, is treated as a mismatch.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
