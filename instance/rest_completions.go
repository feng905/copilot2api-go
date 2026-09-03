package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"copilot-go/config"
)

// ProxyRestRequest performs an authenticated request against the native Copilot
// REST API (api.business.githubcopilot.com) using the raw GitHub token and the
// official Copilot CLI header set. This is the proven way to call the API
// directly (verified against /chat/completions, /responses and /models).
func ProxyRestRequest(ctx context.Context, state *config.State, method, path string, bodyBytes []byte, extraHeaders http.Header) (*http.Response, error) {
	// Use the REST base URL authorized for this token (endpoints.api from the
	// token endpoint), not the account-type heuristic — GitHub may authorize a
	// different host (e.g. api.business.githubcopilot.com) than the one implied
	// by accountType.
	requestURL := config.CopilotRestBaseURL(state) + path
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	for k, v := range config.CopilotRestHeaders(state) {
		req.Header[k] = v
	}
	for k, v := range extraHeaders {
		req.Header[k] = v
	}
	return getStreamingClient().Do(req)
}

// RestDoCompletions proxies an OpenAI-style /chat/completions request to the
// native GitHub Copilot REST API. It drives routing through routeCompletion,
// which prefers the interface the USER called (/chat/completions) and only
// translates to /responses when the model does not support chat. The caller's
// request body is forwarded verbatim whenever the model supports the called
// interface (so the system prompt, messages and tools are never modified).
func RestDoCompletions(ctx context.Context, state *config.State, bodyBytes []byte) (*http.Response, error) {
	return routeCompletion(ctx, state, "chat", bodyBytes)
}

// routeCompletion is the single routing decision for both OpenAI endpoints.
//   - callerInterface is "chat"  when the user called /v1/chat/completions
//     and "responses"            when the user called /v1/responses.
//
// Principle: prefer the interface the user called. Only translate to the other
// protocol when the model does not support the called interface. Concretely:
//  1. model supports callerInterface  -> forward verbatim (no body change)
//  2. model supports the other one     -> translate callerInterface<->other and
//     convert the response back so the client gets its expected format
//  3. unknown model (catalog missing) -> try callerInterface verbatim; if
//     upstream rejects it as unsupported for that interface, fall back to the
//     other interface (translation path)
func routeCompletion(ctx context.Context, state *config.State, callerInterface string, bodyBytes []byte) (*http.Response, error) {
	var probe map[string]interface{}
	_ = json.Unmarshal(bodyBytes, &probe)
	model, _ := probe["model"].(string)
	stream, _ := probe["stream"].(bool)

	targetEP, translate := chooseEndpoint(state, model, callerInterface)
	if !translate {
		resp, err := ProxyRestRequest(ctx, state, "POST", targetEP, bodyBytes, nil)
		if err != nil {
			return nil, err
		}
		// Upstream rejected this interface for the model — fall back to the
		// other protocol via translation.
		if isUnsupportedForInterface(resp, targetEP) {
			_ = resp.Body.Close()
			otherEP := "/responses"
			if targetEP == "/responses" {
				otherEP = "/chat/completions"
			}
			return doTranslation(ctx, state, callerInterface, otherEP, bodyBytes, model, stream)
		}
		return resp, nil
	}
	return doTranslation(ctx, state, callerInterface, targetEP, bodyBytes, model, stream)
}

// chooseEndpoint decides which native endpoint to hit and whether translation
// is required, given the interface the USER called and the model's supported
// endpoints. It is a pure function so the routing policy is unit-testable.
func chooseEndpoint(state *config.State, model, callerInterface string) (targetEP string, translate bool) {
	preferredEP := "/chat/completions"
	otherEP := "/responses"
	if callerInterface == "responses" {
		preferredEP = "/responses"
		otherEP = "/chat/completions"
	}
	// 1. Model supports the interface the user called -> verbatim.
	if supportsEndpoint(state, model, preferredEP) {
		return preferredEP, false
	}
	// 2. Model supports the other interface -> translate.
	if supportsEndpoint(state, model, otherEP) {
		return otherEP, true
	}
	// 3. Unknown (catalog metadata missing): default to the caller's interface
	//    verbatim; the live call may fall back to the other interface.
	return preferredEP, false
}

// doTranslation translates the caller's request from callerInterface to
// targetEP and converts the upstream response back into callerInterface format.
func doTranslation(ctx context.Context, state *config.State, callerInterface, targetEP string, bodyBytes []byte, model string, stream bool) (*http.Response, error) {
	if callerInterface == "chat" {
		// chat caller -> target must be /responses
		return doResponsesTranslation(ctx, state, bodyBytes, model, stream)
	}
	// responses caller -> target must be /chat/completions
	return doChatTranslation(ctx, state, bodyBytes, model, stream)
}

// isUnsupportedForInterface inspects a non-2xx response from a native endpoint
// to decide whether the failure is "this model is not available on this
// interface" (so we should try the other protocol). The body is restored so
// callers can still read it. Detection is precise: it only matches the
// upstream's explicit "model not supported / does not support <interface>"
// errors, NOT a generic substring like "responses" (which caused false
// fallbacks before).
func isUnsupportedForInterface(resp *http.Response, triedEP string) bool {
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		return false
	}
	if resp.Body == nil {
		return false
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	s := string(b)
	return strings.Contains(s, "unsupported_api_for_model") ||
		strings.Contains(s, "does not support")
}

func supportsEndpoint(state *config.State, model, ep string) bool {
	state.RLock()
	defer state.RUnlock()
	if state.Models == nil {
		return false
	}
	for _, m := range state.Models.Data {
		if m.ID == model {
			for _, e := range m.SupportedEndpoints {
				if e == ep {
					return true
				}
			}
			return false
		}
	}
	return false
}

// sanitizeResponsesInputMessage strips fields that the GitHub /responses API
// does not accept. Some OpenAI-compatible clients (and previous assistant turns
// from reasoning models) attach reasoning_content to messages; forwarding it
// verbatim produces "Unknown parameter: 'input[N].reasoning_content'" (HTTP 400).
// Only fields the Responses API understands are retained.
func sanitizeResponsesInputMessage(msg map[string]interface{}) map[string]interface{} {
	delete(msg, "reasoning_content")
	// content parts (multimodal messages) may also carry reasoning_content.
	if content, ok := msg["content"].([]interface{}); ok {
		for _, raw := range content {
			if part, ok := raw.(map[string]interface{}); ok {
				delete(part, "reasoning_content")
			}
		}
	}
	return msg
}

// doResponsesTranslation sends a Chat request to the /responses endpoint and
// converts the response back to Chat format (non-streaming), or wraps it as a
// single SSE chunk when the caller asked for streaming. Used when the caller
// used /chat/completions but the model only supports /responses.
func doResponsesTranslation(ctx context.Context, state *config.State, bodyBytes []byte, model string, stream bool) (*http.Response, error) {
	var chat map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &chat); err != nil {
		return nil, err
	}

	responses := map[string]interface{}{
		"model":  model,
		"stream": false,
	}

	var input []interface{}
	if msgs, ok := chat["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role == "system" {
				responses["instructions"] = msg["content"]
				continue
			}
			input = append(input, sanitizeResponsesInputMessage(msg))
		}
	}
	responses["input"] = input

	for _, k := range []string{"temperature", "top_p", "tools", "tool_choice", "stop", "seed", "reasoning_effort"} {
		if v, ok := chat[k]; ok {
			responses[k] = v
		}
	}
	if v, ok := chat["max_tokens"]; ok {
		responses["max_output_tokens"] = v
	}

	respBody, err := json.Marshal(responses)
	if err != nil {
		return nil, err
	}

	resp, err := ProxyRestRequest(ctx, state, "POST", "/responses", respBody, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream /responses returned %d: %s", resp.StatusCode, string(b))
	}

	if !stream {
		chatJSON, err := translateResponsesToChat(resp, model)
		if err != nil {
			return nil, err
		}
		return respondWith(chatJSON, "application/json"), nil
	}

	// Streaming requested: buffer the (non-streaming) response and emit it as a
	// single SSE chunk so OpenAI clients still receive a valid stream.
	return wrapResponsesAsChatSSE(resp, model)
}

// translateResponsesToChat reads a Responses API JSON body and converts it into
// an OpenAI Chat completion JSON body.
func translateResponsesToChat(resp *http.Response, model string) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r map[string]interface{}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}

	// Account-level response encryption: GitHub returns encrypted_content
	// instead of plaintext for some business/enterprise accounts. The
	// decryption key is only available through the official Copilot CLI's
	// session.create handshake, so such responses cannot be proxied verbatim.
	if output, ok := r["output"].([]interface{}); ok && len(output) > 0 {
		if m, ok := output[0].(map[string]interface{}); ok {
			if _, encrypted := m["encrypted_content"]; encrypted {
				return nil, fmt.Errorf("upstream response for model %q is encrypted (account-level response encryption); it cannot be proxied verbatim — use an account without response encryption, or the official Copilot CLI", model)
			}
		}
	}

	var text strings.Builder
	if output, ok := r["output"].([]interface{}); ok {
		for _, item := range output {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if content, ok := m["content"].([]interface{}); ok {
				for _, c := range content {
					if cm, ok := c.(map[string]interface{}); ok {
						if t, ok := cm["text"].(string); ok {
							text.WriteString(t)
						}
					}
				}
			}
		}
	}

	id, _ := r["id"].(string)
	model = r["model"].(string)

	chat := map[string]interface{}{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": text.String(),
				},
			},
		},
		"usage": r["usage"],
	}
	return json.Marshal(chat)
}

func wrapResponsesAsChatSSE(resp *http.Response, model string) (*http.Response, error) {
	chatJSON, err := translateResponsesToChat(resp, model)
	if err != nil {
		return nil, err
	}
	var chat map[string]interface{}
	_ = json.Unmarshal(chatJSON, &chat)
	content := ""
	if choices, ok := chat["choices"].([]interface{}); ok && len(choices) > 0 {
		if cm, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := cm["message"].(map[string]interface{}); ok {
				content, _ = msg["content"].(string)
			}
		}
	}

	sb := &bytes.Buffer{}
	chunk := map[string]interface{}{
		"id":      chat["id"],
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"role": "assistant", "content": content},
				"finish_reason": nil,
			},
		},
	}
	chunkBytes, _ := json.Marshal(chunk)
	sb.WriteString("data: " + string(chunkBytes) + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return respondWith(sb.Bytes(), "text/event-stream"), nil
}

// doChatTranslation sends a Responses request to the /chat/completions endpoint
// and converts the response back to Responses format. Used when the caller used
// /v1/responses but the model only supports /chat/completions (e.g. Claude).
func doChatTranslation(ctx context.Context, state *config.State, bodyBytes []byte, model string, stream bool) (*http.Response, error) {
	var respBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &respBody); err != nil {
		return nil, err
	}

	chat := translateResponsesRequestToChat(respBody)

	chatBytes, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}

	resp, err := ProxyRestRequest(ctx, state, "POST", "/chat/completions", chatBytes, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream /chat/completions returned %d: %s", resp.StatusCode, string(b))
	}

	if !stream {
		responsesJSON, err := translateChatToResponses(resp, model)
		if err != nil {
			return nil, err
		}
		return respondWith(responsesJSON, "application/json"), nil
	}

	return wrapChatAsResponsesSSE(resp, model)
}

// translateResponsesRequestToChat converts a Responses API request body into an
// OpenAI Chat completion request body.
func translateResponsesRequestToChat(r map[string]interface{}) map[string]interface{} {
	chat := map[string]interface{}{
		"model":  r["model"],
		"stream": r["stream"],
	}
	var messages []interface{}
	if instr, ok := r["instructions"]; ok {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}
	if input, ok := r["input"].([]interface{}); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := item["role"].(string)
			if role == "" {
				if t, _ := item["type"].(string); t == "message" {
					role, _ = item["role"].(string)
				}
			}
			content := item["content"]
			var contentStr interface{}
			switch c := content.(type) {
			case string:
				contentStr = c
			case []interface{}:
				var sb strings.Builder
				for _, p := range c {
					if pm, ok := p.(map[string]interface{}); ok {
						if t, ok := pm["text"].(string); ok {
							sb.WriteString(t)
						}
					}
				}
				contentStr = sb.String()
			default:
				contentStr = content
			}
			messages = append(messages, map[string]interface{}{"role": role, "content": contentStr})
		}
	}
	chat["messages"] = messages
	for _, k := range []string{"temperature", "top_p", "tools", "tool_choice", "stop", "seed", "reasoning_effort"} {
		if v, ok := r[k]; ok {
			chat[k] = v
		}
	}
	if v, ok := r["max_output_tokens"]; ok {
		chat["max_tokens"] = v
	}
	return chat
}

// translateChatToResponses reads a Chat completion JSON body and converts it
// into a Responses API JSON body.
func translateChatToResponses(resp *http.Response, model string) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var c map[string]interface{}
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}

	text := ""
	if choices, ok := c["choices"].([]interface{}); ok && len(choices) > 0 {
		if cm, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := cm["message"].(map[string]interface{}); ok {
				if t, ok := msg["content"].(string); ok {
					text = t
				}
			}
		}
	}

	m, _ := c["model"].(string)
	if m == "" {
		m = model
	}

	responses := map[string]interface{}{
		"id":     c["id"],
		"object": "response",
		"model":  m,
		"output": []map[string]interface{}{
			{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]interface{}{{"type": "output_text", "text": text}},
			},
		},
		"usage": c["usage"],
	}
	return json.Marshal(responses)
}

func wrapChatAsResponsesSSE(resp *http.Response, model string) (*http.Response, error) {
	responsesJSON, err := translateChatToResponses(resp, model)
	if err != nil {
		return nil, err
	}
	var r map[string]interface{}
	_ = json.Unmarshal(responsesJSON, &r)
	text := ""
	if output, ok := r["output"].([]interface{}); ok && len(output) > 0 {
		if m, ok := output[0].(map[string]interface{}); ok {
			if content, ok := m["content"].([]interface{}); ok && len(content) > 0 {
				if cm, ok := content[0].(map[string]interface{}); ok {
					text, _ = cm["text"].(string)
				}
			}
		}
	}

	sb := &bytes.Buffer{}
	id, _ := r["id"].(string)
	// Emit one output_text delta with the full text, then a completed event.
	delta := map[string]interface{}{
		"type": "response.output_text.delta",
		"item_id": "msg_0",
		"output_index": 0,
		"content_index": 0,
		"delta": text,
	}
	dBytes, _ := json.Marshal(delta)
	sb.WriteString("data: " + string(dBytes) + "\n\n")

	completed := map[string]interface{}{
		"type":   "response.completed",
		"id":     id,
		"object": "response",
		"model":  model,
		"output": r["output"],
		"usage":  r["usage"],
	}
	cBytes, _ := json.Marshal(completed)
	sb.WriteString("data: " + string(cBytes) + "\n\n")
	return respondWith(sb.Bytes(), "text/event-stream"), nil
}

func respondWith(body []byte, contentType string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
