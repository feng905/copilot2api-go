package instance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

// withTempStoreConfig redirects the store to a temp dir and optionally seeds
// response-header-config.json with the given raw JSON.
func withTempStoreConfig(t *testing.T, raw string) {
	t.Helper()
	old := store.AppDir
	store.AppDir = t.TempDir()
	t.Cleanup(func() { store.AppDir = old })
	if raw != "" {
		if err := os.WriteFile(store.ResponseHeaderConfigFile(), []byte(raw), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func upstreamWithHeaders(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// TestForwardingDefaultWhitelistDirectPath drives ForwardCompletionsResponse
// with a mock upstream carrying a mix of headers, under the shipped default
// whitelist (no config file).
func TestForwardingDefaultWhitelistDirectPath(t *testing.T) {
	withTempStoreConfig(t, "")
	gin.SetMode(gin.TestMode)

	upstream := upstreamWithHeaders(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Request-Id", "upstream-abc")
		h.Set("Anthropic-Ratelimit-Requests-Limit", "50")
		h.Set("Set-Cookie", "session=xyz")
		h.Set("X-Custom", "nope")
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/chat/completions", nil)

	ForwardCompletionsResponse(c, resp)

	if got := w.Header().Get("Request-Id"); got != "upstream-abc" {
		t.Fatalf("Request-Id should be forwarded under default whitelist, got %q", got)
	}
	if got := w.Header().Get("Anthropic-Ratelimit-Requests-Limit"); got != "50" {
		t.Fatalf("Anthropic ratelimit header should be forwarded, got %q", got)
	}
	if got := w.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie must never be forwarded, got %q", got)
	}
	if got := w.Header().Get("X-Custom"); got != "" {
		t.Fatalf("unlisted header must not be forwarded under whitelist, got %q", got)
	}
	// Our own Content-Type must win over the upstream's.
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("our Content-Type should win, got %q", ct)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body should be intact: %v", err)
	}
}

// TestForwardingPassthroughKeepsExclusions verifies passthrough forwards
// unlisted headers but still drops the always-excluded set.
func TestForwardingPassthroughKeepsExclusions(t *testing.T) {
	withTempStoreConfig(t, `{"mode":"passthrough","whitelist":[],"blacklist":[]}`)
	gin.SetMode(gin.TestMode)

	upstream := upstreamWithHeaders(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Custom", "yes")
		h.Set("Connection", "close")
		h.Set("Set-Cookie", "session=xyz")
		h.Set("Retry-After", "7")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/chat/completions", nil)

	ForwardCompletionsResponse(c, resp)

	if got := w.Header().Get("X-Custom"); got != "yes" {
		t.Fatalf("passthrough should forward unlisted headers, got %q", got)
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("passthrough should forward Retry-After, got %q", got)
	}
	for _, h := range []string{"Set-Cookie", "Connection"} {
		if got := w.Header().Get(h); got != "" {
			t.Fatalf("%s must never be forwarded even in passthrough, got %q", h, got)
		}
	}
}

// TestForwardingBlacklist verifies blacklist mode forwards everything except
// the listed headers.
func TestForwardingBlacklist(t *testing.T) {
	withTempStoreConfig(t, `{"mode":"blacklist","whitelist":[],"blacklist":["x-custom"]}`)
	gin.SetMode(gin.TestMode)

	upstream := upstreamWithHeaders(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Custom", "blocked")
		h.Set("Server", "upstream-srv")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/chat/completions", nil)

	ForwardCompletionsResponse(c, resp)

	if got := w.Header().Get("X-Custom"); got != "" {
		t.Fatalf("blacklisted header must not be forwarded, got %q", got)
	}
	if got := w.Header().Get("Server"); got != "upstream-srv" {
		t.Fatalf("unlisted header should be forwarded under blacklist, got %q", got)
	}
}

// TestForwardingTranslatedMessagesErrorBranch verifies the messages translation
// path forwards headers even when relaying an upstream error (the c.Data
// branch of handleAnthropicNonStream).
func TestForwardingTranslatedMessagesErrorBranch(t *testing.T) {
	// Explicit whitelist including openai-processing-ms (not in the shipped
	// default list), proving config loading works on the error branch too.
	withTempStoreConfig(t, `{"mode":"whitelist","whitelist":["openai-processing-ms"],"blacklist":[]}`)
	gin.SetMode(gin.TestMode)

	upstream := upstreamWithHeaders(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Openai-Processing-Ms", "42")
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"payment required"}}`))
	})
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)

	handleAnthropicNonStream(c, resp)

	if got := w.Header().Get("Openai-Processing-Ms"); got != "42" {
		t.Fatalf("error branch should forward whitelisted headers, got %q", got)
	}
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status should relay, got %d", w.Code)
	}
}
