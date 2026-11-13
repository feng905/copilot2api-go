package instance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"copilot-go/config"

	"github.com/gin-gonic/gin"
)

// TestDoMessagesProxyDirectPathVerbatimPassThrough verifies the direct
// /v1/messages path forwards the client's original bytes untouched — no
// struct round-trip that would silently drop fields like thinking.
func TestDoMessagesProxyDirectPathVerbatimPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	state := config.NewState()
	state.RestEndpoint = upstream.URL
	state.Models = &config.ModelsResponse{Data: []config.ModelEntry{{
		ID:                 "claude-sonnet-4",
		SupportedEndpoints: []string{"/v1/messages"},
	}}}

	// thinking and an unknown future parameter must both survive the proxy.
	body := []byte(`{"model":"claude-sonnet-4","max_tokens":64,"stream":false,"thinking":{"type":"enabled","budget_tokens":8000},"some_future_param":{"x":1},"messages":[{"role":"user","content":"hi"}]}`)

	w := httptest.NewRecorder()
	gotBody = nil
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	resp, direct, err := DoMessagesProxy(c, state, body)
	if err != nil {
		t.Fatalf("DoMessagesProxy: %v", err)
	}
	defer resp.Body.Close()
	if !direct {
		t.Fatal("expected direct proxy path")
	}
	if string(gotBody) != string(body) {
		t.Fatalf("upstream did not receive original bytes verbatim:\nsent:     %s\nexpected: %s", gotBody, body)
	}
}

// TestDoMessagesProxyDirectPathEditsOnlyModelAndMaxTokens verifies that when
// the model needs renaming or max_tokens is missing, only those two fields are
// changed and everything else (thinking, unknown params) is preserved.
func TestDoMessagesProxyDirectPathEditsOnlyModelAndMaxTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	state := config.NewState()
	state.RestEndpoint = upstream.URL
	state.Models = &config.ModelsResponse{Data: []config.ModelEntry{{
		ID:                 "claude-opus-4",
		SupportedEndpoints: []string{"/v1/messages"},
		Capabilities: &config.ModelCapabilities{
			Limits: config.ModelLimits{MaxOutputTokens: 12345},
		},
	}}}

	body := []byte(`{"model":"claude-opus-4-20250514","stream":false,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"hi"}]}`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	resp, direct, err := DoMessagesProxy(c, state, body)
	if err != nil {
		t.Fatalf("DoMessagesProxy: %v", err)
	}
	defer resp.Body.Close()
	if !direct {
		t.Fatal("expected direct proxy path")
	}

	var sent map[string]interface{}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("parse sent body: %v (%s)", err, gotBody)
	}
	if sent["model"] != "claude-opus-4" {
		t.Fatalf("expected model renamed to claude-opus-4, got %v", sent["model"])
	}
	if sent["max_tokens"] != float64(12345) {
		t.Fatalf("expected max_tokens auto-filled to 12345, got %v", sent["max_tokens"])
	}
	thinking, ok := sent["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8000) {
		t.Fatalf("thinking param lost or altered: %v", sent["thinking"])
	}
}
