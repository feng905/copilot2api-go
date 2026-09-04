package instance

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
	return m
}

// TestNormalizeResponsesInput_StringContentUpgraded reproduces the real relay
// record 20260904130502725835000t53s3wRw: new-api converts chat messages to
// input items with STRING content (valid OpenAI EasyInputMessage), and GitHub
// /responses rejects them with "Invalid type for 'input': expected a string,
// but got an object instead." All string items must become typed parts.
func TestNormalizeResponsesInput_StringContentUpgraded(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","stream":true,"input":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"Hi! How can I help you today?"},` +
		`{"role":"user","content":"你擅长什么"},` +
		`{"role":"assistant","content":"..."},` +
		`{"role":"user","content":"hi"}]}`

	out := normalizeResponsesInput([]byte(in))
	body := decodeJSON(t, string(out))
	input := body["input"].([]interface{})

	user1 := input[0].(map[string]interface{})
	if _, has := user1["type"]; has {
		t.Fatalf("user item must not gain a type: %v", user1)
	}
	part := user1["content"].([]interface{})[0].(map[string]interface{})
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Fatalf("user content = %v, want [{type:input_text,text:hi}]", user1["content"])
	}

	asst := input[1].(map[string]interface{})
	if asst["type"] != "message" || asst["status"] != "completed" {
		t.Fatalf("assistant item = %v, want type=message status=completed", asst)
	}
	apart := asst["content"].([]interface{})[0].(map[string]interface{})
	if apart["type"] != "output_text" || apart["text"] != "Hi! How can I help you today?" {
		t.Fatalf("assistant content = %v, want output_text part", asst["content"])
	}

	if body["model"] != "gpt-5.6-luna" || body["stream"] != true {
		t.Fatalf("unrelated fields mangled: %v %v", body["model"], body["stream"])
	}
}

// TestNormalizeResponsesInput_StoreCoerced reproduces the racoder (VS Code)
// failure: the client sends "store": true and GitHub /responses rejects it with
// 400 {"code":"unsupported_value","param":"store","message":"store is not
// supported"} — while "store": false is accepted (verified against the real
// upstream, see TestNormalizeResponsesInput_StrictBodyUnchanged). true must be
// coerced to false, with every other field preserved.
func TestNormalizeResponsesInput_StoreCoerced(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","stream":true,"store":true,` +
		`"truncation":"disabled","include":["reasoning.encrypted_content"],` +
		`"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))

	if out["store"] != false {
		t.Fatalf("store = %v, want false", out["store"])
	}
	if out["truncation"] != "disabled" {
		t.Fatalf("truncation = %v, want disabled", out["truncation"])
	}
	if parts, ok := out["include"].([]interface{}); !ok || len(parts) != 1 || parts[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v, want preserved", out["include"])
	}
}

// TestNormalizeResponsesInput_StrictBodyUnchanged: a body already in the shape
// the upstream demonstrably accepts (Cherry Studio's successful /v1/responses
// request) must come back byte-for-byte identical.
func TestNormalizeResponsesInput_StrictBodyUnchanged(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","stream":true,"include":["reasoning.encrypted_content"],` +
		`"store":false,` +
		`"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"},` +
		`{"content":[{"text":"...","type":"output_text"}],"role":"assistant","status":"completed","type":"message"},` +
		`{"content":[{"text":"hi","type":"input_text"}],"role":"user"}]}`
	if got := normalizeResponsesInput([]byte(in)); string(got) != in {
		t.Fatalf("strict body rewritten:\n got: %s\nwant: %s", got, in)
	}
}

// TestNormalizeResponsesInput_TypedItemsUntouched: non-message items
// (function_call, reasoning with encrypted_content) must not be modified.
func TestNormalizeResponsesInput_TypedItemsUntouched(t *testing.T) {
	in := `{"model":"m","input":[{"type":"function_call","call_id":"fc_1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"fc_1","output":"ok"},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"enc","summary":[]}]}`
	if got := normalizeResponsesInput([]byte(in)); string(got) != in {
		t.Fatalf("typed items rewritten:\n got: %s\nwant: %s", got, in)
	}
}

// TestNormalizeResponsesInput_ChatPartTypesRemapped: array content using the
// chat-style {"type":"text"} part (what doResponsesTranslation forwards
// verbatim for multimodal messages) must be remapped per role.
func TestNormalizeResponsesInput_ChatPartTypesRemapped(t *testing.T) {
	in := `{"input":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"c"}],"reasoning_content":"r"}]}`
	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))
	input := out["input"].([]interface{})

	user := input[0].(map[string]interface{})
	for _, raw := range user["content"].([]interface{}) {
		if raw.(map[string]interface{})["type"] != "input_text" {
			t.Fatalf("user part type = %v, want input_text", raw)
		}
	}
	asst := input[1].(map[string]interface{})
	if raw := asst["content"].([]interface{})[0].(map[string]interface{}); raw["type"] != "output_text" {
		t.Fatalf("assistant part type = %v, want output_text", raw)
	}
	if _, has := asst["reasoning_content"]; has {
		t.Fatal("reasoning_content must be stripped from input items")
	}
	if asst["type"] != "message" || asst["status"] != "completed" {
		t.Fatalf("assistant item = %v, want type=message status=completed", asst)
	}
}

// TestNormalizeResponsesInput_Guards: non-array input, string input and invalid
// JSON pass through unchanged.
func TestNormalizeResponsesInput_Guards(t *testing.T) {
	cases := []string{
		`{"model":"m"}`,                         // no input
		`{"model":"m","input":"just a string"}`, // string input is valid upstream
		`not json at all`,
	}
	for _, in := range cases {
		if got := normalizeResponsesInput([]byte(in)); string(got) != in {
			t.Fatalf("input %q rewritten to %q; must pass through", in, got)
		}
	}
}

// TestNormalizeResponsesInput_TranslationIntermediateShape: doResponsesTranslation
// copies chat messages into input verbatim (string content) and extracts system
// messages into instructions before normalizing — that exact intermediate shape
// must come out strict, with instructions and pass-through params preserved.
func TestNormalizeResponsesInput_TranslationIntermediateShape(t *testing.T) {
	in := `{"instructions":"You are helpful.","model":"gpt-5.6-luna","stream":false,` +
		`"temperature":0.7,` +
		`"input":[{"content":"hi","role":"user"},` +
		`{"content":"Hi! How can I help you today?","role":"assistant"}]}`
	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))

	if out["instructions"] != "You are helpful." {
		t.Fatalf("instructions = %v, want preserved", out["instructions"])
	}
	if out["temperature"] != 0.7 || out["stream"] != false {
		t.Fatalf("pass-through params mangled: %v %v", out["temperature"], out["stream"])
	}
	input := out["input"].([]interface{})
	if part := input[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{}); part["type"] != "input_text" {
		t.Fatalf("user content = %v, want input_text part", input[0])
	}
	asst := input[1].(map[string]interface{})
	if part := asst["content"].([]interface{})[0].(map[string]interface{}); part["type"] != "output_text" {
		t.Fatalf("assistant content = %v, want output_text part", asst)
	}
	if asst["type"] != "message" || asst["status"] != "completed" {
		t.Fatalf("assistant item = %v, want type=message status=completed", asst)
	}
	if !strings.Contains(string(mustJSON(t, out)), "You are helpful.") {
		t.Fatal("instructions lost in normalized body")
	}
}

// TestNormalizeResponsesInput_FlattensChatTools reproduces the WorkBuddy
// failure (relay record 20260904133617815851000bHmBWvq1): the chat request
// carries function tools in the nested Chat shape; forwarded verbatim to
// /responses, upstream rejects it with "Missing required parameter:
// 'tools[0].name'". Nested definitions and tool_choice must be flattened,
// keeping the parameters schema untouched.
func TestNormalizeResponsesInput_FlattensChatTools(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","stream":true,` +
		`"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],` +
		`"tools":[{"type":"function","function":{"name":"mcp__ardot__batch_edit",` +
		`"description":"Create/Update/Move/Delete nodes.",` +
		`"parameters":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",` +
		`"properties":{"operations":{"type":"string"}},"required":["operations"],` +
		`"additionalProperties":true}}},` +
		`{"type":"function","function":{"name":"present_files","parameters":{"type":"object"}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"present_files"}}}`

	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))
	tools := out["tools"].([]interface{})

	first := tools[0].(map[string]interface{})
	if _, has := first["function"]; has {
		t.Fatal("tools[0] still carries the nested function object")
	}
	if first["type"] != "function" || first["name"] != "mcp__ardot__batch_edit" {
		t.Fatalf("tools[0] = %v, want flat function tool", first)
	}
	params := first["parameters"].(map[string]interface{})
	if params["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Fatalf("parameters schema mangled: %v", params)
	}
	if _, has := params["additionalProperties"]; !has {
		t.Fatal("parameters lost additionalProperties")
	}

	second := tools[1].(map[string]interface{})
	if second["name"] != "present_files" {
		t.Fatalf("tools[1].name = %v, want present_files", second["name"])
	}

	tc := out["tool_choice"].(map[string]interface{})
	if _, has := tc["function"]; has {
		t.Fatal("tool_choice still carries the nested function object")
	}
	if tc["type"] != "function" || tc["name"] != "present_files" {
		t.Fatalf("tool_choice = %v, want {type:function,name:present_files}", tc)
	}
}

// TestNormalizeResponsesInput_FlatToolsUntouched: tools already in the
// /responses flat shape and string tool_choice must pass through untouched
// (while input items still get upgraded alongside them).
func TestNormalizeResponsesInput_FlatToolsUntouched(t *testing.T) {
	in := `{"model":"m","input":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],` +
		`"tool_choice":"auto"}`
	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))

	tool := out["tools"].([]interface{})[0].(map[string]interface{})
	if _, has := tool["function"]; has {
		t.Fatal("flat tool must not gain a function object")
	}
	if tool["name"] != "f" || tool["description"] != "d" {
		t.Fatalf("flat tool mangled: %v", tool)
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", out["tool_choice"])
	}
	part := out["input"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Fatalf("input normalization skipped: %v", part)
	}
}

// TestNormalizeResponsesInput_ToolsWithoutInput: a /responses body without an
// input array (e.g. built by another relay) still gets chat-shaped tools
// flattened.
func TestNormalizeResponsesInput_ToolsWithoutInput(t *testing.T) {
	in := `{"model":"m","tools":[{"type":"function","function":{"name":"f"}}]}`
	out := decodeJSON(t, string(normalizeResponsesInput([]byte(in))))
	tool := out["tools"].([]interface{})[0].(map[string]interface{})
	if tool["name"] != "f" {
		t.Fatalf("tools[0] = %v, want flattened name", tool)
	}
	if _, has := tool["function"]; has {
		t.Fatal("nested function object not removed")
	}
}

func mustJSON(t *testing.T, m map[string]interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
