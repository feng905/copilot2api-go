package instance

import (
	"net/http"
	"strings"

	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

// alwaysExcludedHeaders are never forwarded in ANY mode: hop-by-hop headers,
// proxy-controlled values, and Set-Cookie (forwarding upstream Copilot session
// cookies to API clients would leak credentials and poison cookie jars).
// Keys are in canonical http.CanonicalHeaderKey form.
var alwaysExcludedHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Type":        true,
	"Content-Length":      true,
	"Set-Cookie":          true,
}

// headerForwardAllowed reports whether an upstream response header may be
// copied to the client under cfg. Matching is case-insensitive in both
// directions (stored list vs upstream name); the always-excluded set wins over
// every mode including passthrough.
func headerForwardAllowed(name string, cfg store.ResponseHeaderConfig) bool {
	if alwaysExcludedHeaders[http.CanonicalHeaderKey(name)] {
		return false
	}
	switch cfg.Mode {
	case store.HeaderModePassthrough:
		return true
	case store.HeaderModeWhitelist:
		return containsFold(cfg.Whitelist, name)
	case store.HeaderModeBlacklist:
		return !containsFold(cfg.Blacklist, name)
	default:
		// Unknown mode (hand-edited config): fail closed to the shipped
		// whitelist semantics.
		return containsFold(cfg.Whitelist, name)
	}
}

func containsFold(list []string, name string) bool {
	for _, entry := range list {
		if strings.EqualFold(entry, name) {
			return true
		}
	}
	return false
}

// ApplyUpstreamResponseHeaders copies eligible headers from the upstream
// response onto the client response with Add (multi-value preserved). Must be
// called BEFORE c.Status / the first body byte — gin freezes the header map at
// first write. The callers' fixed header sets are applied afterwards with Set,
// so proxy-controlled values (Content-Type, Cache-Control, …) always win over
// forwarded ones.
func ApplyUpstreamResponseHeaders(c *gin.Context, upstream http.Header) {
	cfg, err := store.GetResponseHeaderConfig()
	if err != nil || cfg.Mode == "" {
		cfg = store.DefaultResponseHeaderConfig()
	}
	out := c.Writer.Header()
	for name, values := range upstream {
		if !headerForwardAllowed(name, cfg) {
			continue
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}
}
