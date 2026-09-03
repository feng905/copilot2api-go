package instance

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"copilot-go/config"
)

// buildState builds a config.State with a fixed model catalog for routing tests.
func buildState(models []config.ModelEntry) *config.State {
	return &config.State{Models: &config.ModelsResponse{Object: "list", Data: models}}
}

func ep(t *testing.T, state *config.State, model, caller string) (target string, translate bool) {
	t.Helper()
	return chooseEndpoint(state, model, caller)
}

func TestChooseEndpoint_PrefersCallerInterface(t *testing.T) {
	state := buildState([]config.ModelEntry{
		{ID: "claude-opus-5", SupportedEndpoints: []string{"/chat/completions"}},                 // chat-only
		{ID: "gpt-5.6-sol", SupportedEndpoints: []string{"/responses"}},                          // responses-only
		{ID: "gpt-5.4", SupportedEndpoints: []string{"/chat/completions", "/responses"}},         // both
		{ID: "gpt-4o"},                                                                          // unknown (no endpoints)
	})

	cases := []struct {
		name     string
		model    string
		caller   string
		wantEP   string
		wantXlat bool
	}{
		// --- chat caller ---
		{"chat: claude is chat-only -> chat verbatim", "claude-opus-5", "chat", "/chat/completions", false},
		{"chat: gpt-5.6-sol is responses-only -> responses translate", "gpt-5.6-sol", "chat", "/responses", true},
		{"chat: gpt-5.4 supports both -> chat verbatim", "gpt-5.4", "chat", "/chat/completions", false},
		{"chat: gpt-4o unknown -> default chat verbatim", "gpt-4o", "chat", "/chat/completions", false},
		// --- responses caller (symmetric) ---
		{"responses: gpt-5.6-sol is responses-only -> responses verbatim", "gpt-5.6-sol", "responses", "/responses", false},
		{"responses: claude is chat-only -> chat translate", "claude-opus-5", "responses", "/chat/completions", true},
		{"responses: gpt-5.4 supports both -> responses verbatim", "gpt-5.4", "responses", "/responses", false},
		{"responses: gpt-4o unknown -> default responses verbatim", "gpt-4o", "responses", "/responses", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEP, gotXlat := ep(t, state, c.model, c.caller)
			if gotEP != c.wantEP || gotXlat != c.wantXlat {
				t.Fatalf("model=%s caller=%s: got (%s, translate=%v), want (%s, translate=%v)",
					c.model, c.caller, gotEP, gotXlat, c.wantEP, c.wantXlat)
			}
		})
	}
}

// TestChooseEndpoint_KeyInvariant: a model that supports /chat/completions must
// NEVER be translated when the user called /v1/chat/completions. This is the
// exact bug the user reported ("I called /chat/completions but you used /responses").
func TestChooseEndpoint_KeyInvariant(t *testing.T) {
	state := buildState([]config.ModelEntry{
		{ID: "claude-opus-5", SupportedEndpoints: []string{"/chat/completions"}},
		{ID: "gemini-flash", SupportedEndpoints: []string{"/chat/completions"}},
		{ID: "gpt-4o", SupportedEndpoints: []string{"/chat/completions"}},
	})
	for _, m := range []string{"claude-opus-5", "gemini-flash", "gpt-4o"} {
		ep, xlat := chooseEndpoint(state, m, "chat")
		if ep != "/chat/completions" || xlat {
			t.Fatalf("chat-capable model %s called via /chat/completions was routed to %s (translate=%v); must stay on /chat/completions verbatim", m, ep, xlat)
		}
	}
}

func TestIsUnsupportedForInterface_Precise(t *testing.T) {
	chatOnlyOnResponses := newFakeResp(400, `{"error":{"message":"model claude-opus-5 does not support Responses API.","code":"unsupported_api_for_model"}}`)
	if !isUnsupportedForInterface(chatOnlyOnResponses, "/responses") {
		t.Fatal("expected /responses call for a chat-only model to be detected as unsupported")
	}

	responsesOnlyOnChat := newFakeResp(400, `{"error":{"message":"Model gpt-5.6-sol is not available on /chat/completions","code":"unsupported_api_for_model"}}`)
	if !isUnsupportedForInterface(responsesOnlyOnChat, "/chat/completions") {
		t.Fatal("expected /chat/completions call for a responses-only model to be detected as unsupported")
	}

	// A generic 400 that is NOT about interface support must NOT trigger fallback.
	other400 := newFakeResp(400, `{"error":{"message":"Invalid request: missing content","code":"invalid_request"}}`)
	if isUnsupportedForInterface(other400, "/chat/completions") {
		t.Fatal("a non-interface 400 must NOT be treated as unsupported-for-interface (avoids false translation)")
	}

	// The old bug: an error mentioning 'responses' for a chat-capable model used
	// to force a /responses translation. Now only explicit support errors match.
	mentionsResponses := newFakeResp(400, `{"error":{"message":"some other error referencing responses endpoint config"}}`)
	if isUnsupportedForInterface(mentionsResponses, "/chat/completions") {
		t.Fatal("mentioning 'responses' alone must NOT trigger fallback")
	}
}

// minimal helpers to build *http.Response for tests
func newFakeResp(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}
