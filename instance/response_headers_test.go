package instance

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

func TestHeaderForwardAllowed(t *testing.T) {
	whitelist := store.ResponseHeaderConfig{
		Mode:      store.HeaderModeWhitelist,
		Whitelist: []string{"retry-after", "X-Request-Id"},
	}
	blacklist := store.ResponseHeaderConfig{
		Mode:      store.HeaderModeBlacklist,
		Blacklist: []string{"x-custom"},
	}
	passthrough := store.ResponseHeaderConfig{Mode: store.HeaderModePassthrough}

	cases := []struct {
		name    string
		cfg     store.ResponseHeaderConfig
		header  string
		allowed bool
	}{
		{"whitelist hit", whitelist, "retry-after", true},
		{"whitelist hit case-insensitive stored", whitelist, "X-REQUEST-ID", true},
		{"whitelist miss", whitelist, "x-custom", false},
		{"whitelist hit case-insensitive upstream", whitelist, "RETRY-AFTER", true},
		{"blacklist hit", blacklist, "X-Custom", false},
		{"blacklist miss", blacklist, "retry-after", true},
		{"passthrough allow", passthrough, "anything", true},
		{"passthrough excluded content-type", passthrough, "Content-Type", false},
		{"whitelist excluded wins over allowlist", whitelist, "Content-Type", false},
		{"blacklist excluded wins over not-listed", blacklist, "Transfer-Encoding", false},
		{"passthrough excluded set-cookie", passthrough, "Set-Cookie", false},
		{"passthrough excluded connection", passthrough, "Connection", false},
		{"passthrough excluded content-length", passthrough, "Content-Length", false},
		{"passthrough excluded te", passthrough, "TE", false},
		{"passthrough excluded upgrade", passthrough, "Upgrade", false},
		{"passthrough excluded trailer", passthrough, "Trailer", false},
		{"passthrough excluded keep-alive", passthrough, "Keep-Alive", false},
		{"passthrough excluded proxy-auth", passthrough, "Proxy-Authenticate", false},
		{"passthrough excluded proxy-authorization", passthrough, "Proxy-Authorization", false},
		{"unknown mode falls back to whitelist", store.ResponseHeaderConfig{Mode: "", Whitelist: []string{"x-ok"}}, "x-ok", true},
		{"unknown mode falls back to whitelist miss", store.ResponseHeaderConfig{Mode: "", Whitelist: []string{"x-ok"}}, "x-nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerForwardAllowed(tc.header, tc.cfg); got != tc.allowed {
				t.Fatalf("headerForwardAllowed(%q, mode=%s) = %v, want %v", tc.header, tc.cfg.Mode, got, tc.allowed)
			}
		})
	}
}

func TestApplyUpstreamResponseHeadersMultiValue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Temporarily point the store at an empty dir with an explicit whitelist
	// containing X-Multi, without touching real user config.
	old := store.AppDir
	store.AppDir = t.TempDir()
	t.Cleanup(func() { store.AppDir = old })
	cfg := `{"mode":"whitelist","whitelist":["X-Multi"],"blacklist":[]}`
	if err := os.WriteFile(store.ResponseHeaderConfigFile(), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	upstream := http.Header{}
	upstream.Add("X-Multi", "first")
	upstream.Add("X-Multi", "second")
	upstream.Add("X-Multi", "third")
	upstream.Add("Content-Type", "text/event-stream") // always excluded

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ApplyUpstreamResponseHeaders(c, upstream)

	got := c.Writer.Header().Values("X-Multi")
	if len(got) != 3 || got[0] != "first" || got[2] != "third" {
		t.Fatalf("multi-value not preserved: %v", got)
	}
	if len(c.Writer.Header().Values("Content-Type")) != 0 {
		t.Fatalf("Content-Type must never be forwarded, got %v", c.Writer.Header().Values("Content-Type"))
	}
}

func TestApplyUpstreamResponseHeadersFailOpenToDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := store.AppDir
	store.AppDir = t.TempDir() // no config file → shipped default whitelist
	t.Cleanup(func() { store.AppDir = old })

	upstream := http.Header{}
	upstream.Set("Request-Id", "upstream-abc")
	upstream.Set("X-Custom", "nope")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ApplyUpstreamResponseHeaders(c, upstream)

	if got := c.Writer.Header().Get("Request-Id"); got != "upstream-abc" {
		t.Fatalf("default whitelist should forward Request-Id, got %q", got)
	}
	if got := c.Writer.Header().Get("X-Custom"); got != "" {
		t.Fatalf("default whitelist must not forward unlisted headers, got %q", got)
	}
}
