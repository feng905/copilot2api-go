package instance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// --- SSE id normalization ---------------------------------------------------

// feedFrames runs each frame through the normalizer and returns the processed
// frames together with their parsed data payloads.
func feedFrames(t *testing.T, frames []string) ([]string, []map[string]interface{}) {
	t.Helper()
	n := newResponsesStreamNormalizer()
	out := make([]string, 0, len(frames))
	events := make([]map[string]interface{}, 0, len(frames))
	for _, f := range frames {
		processed := string(n.processFrame([]byte(f)))
		out = append(out, processed)
		var ev map[string]interface{}
		for _, line := range strings.Split(processed, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if payload == "" || !json.Valid([]byte(payload)) {
				continue
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatalf("frame produced invalid JSON payload %q: %v", payload, err)
			}
			events = append(events, ev)
			break
		}
	}
	return out, events
}

func eventByType(t *testing.T, events []map[string]interface{}, typ string) map[string]interface{} {
	t.Helper()
	for _, ev := range events {
		if ev["type"] == typ {
			return ev
		}
	}
	t.Fatalf("no %s event in stream", typ)
	return nil
}

// TestResponsesStreamNormalizer_RotatingIDs mirrors the real Copilot
// business-account stream (relay record 20260904123642223466000HTdk3xrK): every
// event carries a freshly encrypted id for the same logical item, which crashes
// clients that correlate events by item id. All references must be pinned to
// the output_item.added ids.
func TestResponsesStreamNormalizer_RotatingIDs(t *testing.T) {
	// Each id below is unique on purpose, mimicking upstream rotation.
	frames := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_rotating_1\",\"output\":[]}}\n\n",
		"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":{\"id\":\"resp_rotating_2\",\"output\":[]}}\n\n",
		// reasoning item added: id "item_rs_added" is the canonical id for output_index 0.
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":2,\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"item_rs_added\",\"encrypted_content\":\"enc\",\"summary\":[]}}\n\n",
		// reasoning item done carries a DIFFERENT id (this is the AI SDK crash site).
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":3,\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"item_rs_done_rotated\",\"encrypted_content\":\"enc\",\"summary\":[]}}\n\n",
		// message item added: canonical id for output_index 1.
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":4,\"output_index\":1,\"item\":{\"type\":\"message\",\"id\":\"item_msg_added\",\"role\":\"assistant\",\"phase\":\"final_answer\"}}\n\n",
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":5,\"output_index\":1,\"item_id\":\"item_cp_rotated\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":6,\"output_index\":1,\"item_id\":\"item_delta_rotated\",\"delta\":\"Hi\"}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":7,\"output_index\":1,\"item_id\":\"item_delta2_rotated\",\"delta\":\"!\"}\n\n",
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":8,\"output_index\":1,\"item\":{\"type\":\"message\",\"id\":\"item_msg_done_rotated\",\"role\":\"assistant\",\"status\":\"completed\"}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":9,\"response\":{\"id\":\"resp_rotating_3\",\"output\":[{\"type\":\"reasoning\",\"id\":\"item_rs_completed_rotated\",\"encrypted_content\":\"enc\",\"summary\":[]},{\"type\":\"message\",\"id\":\"item_msg_completed_rotated\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hi!\"}]}],\"usage\":{\"input_tokens\":45,\"output_tokens\":37}}}\n\n",
	}

	_, events := feedFrames(t, frames)

	if got := eventByType(t, events, "response.output_item.done")["item"].(map[string]interface{})["id"]; got != "item_rs_added" {
		t.Fatalf("reasoning output_item.done id = %v, want item_rs_added (the added id)", got)
	}
	// The second done event (message, output_index 1) must be pinned too.
	msgDone := 0
	for _, ev := range events {
		if ev["type"] == "response.output_item.done" && ev["output_index"] == float64(1) {
			msgDone++
			if got := ev["item"].(map[string]interface{})["id"]; got != "item_msg_added" {
				t.Fatalf("message output_item.done id = %v, want item_msg_added", got)
			}
		}
	}
	if msgDone != 1 {
		t.Fatalf("expected exactly one done event for output_index 1, got %d", msgDone)
	}
	for _, ev := range events {
		if ev["type"] == "response.content_part.added" || ev["type"] == "response.output_text.delta" {
			if got := ev["item_id"]; got != "item_msg_added" {
				t.Fatalf("%s item_id = %v, want item_msg_added", ev["type"], got)
			}
		}
	}
	completed := eventByType(t, events, "response.completed")["response"].(map[string]interface{})
	if got := completed["id"]; got != "resp_rotating_1" {
		t.Fatalf("completed response.id = %v, want resp_rotating_1 (the created id)", got)
	}
	output := completed["output"].([]interface{})
	if got := output[0].(map[string]interface{})["id"]; got != "item_rs_added" {
		t.Fatalf("completed output[0].id = %v, want item_rs_added", got)
	}
	if got := output[1].(map[string]interface{})["id"]; got != "item_msg_added" {
		t.Fatalf("completed output[1].id = %v, want item_msg_added", got)
	}
}

// TestResponsesStreamNormalizer_ConsistentIDsAreByteIdentical: when upstream
// already emits consistent ids (personal accounts, any spec-compliant server)
// the normalizer must not touch a single byte.
func TestResponsesStreamNormalizer_ConsistentIDsAreByteIdentical(t *testing.T) {
	frames := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n",
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":2,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"output_index\":0,\"item_id\":\"msg_1\",\"delta\":\"Hello\"}\n\n",
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":4,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"completed\"}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}]}}\n\n",
	}

	out, _ := feedFrames(t, frames)
	for i, got := range out {
		if got != frames[i] {
			t.Fatalf("frame %d rewritten for an id-consistent stream:\n got: %s\nwant: %s", i, got, frames[i])
		}
	}
}

// TestResponsesStreamNormalizer_PassthroughGuards: frames without exactly one
// parseable JSON data line must pass through untouched.
func TestResponsesStreamNormalizer_PassthroughGuards(t *testing.T) {
	n := newResponsesStreamNormalizer()
	cases := []string{
		"data: [DONE]\n\n",
		"event: response.created\ndata: not json\n\n",                          // invalid JSON
		"event: keep\ndata: {\"a\":1}\ndata: {\"b\":2}\n\n",                    // multiple data lines
		": comment only\n\n",                                                   // SSE comment
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n", // no trailing blank line (EOF partial)
	}
	for _, frame := range cases {
		if got := n.processFrame([]byte(frame)); !bytes.Equal(got, []byte(frame)) {
			t.Fatalf("frame %q rewritten to %q; must pass through byte-for-byte", frame, got)
		}
	}
}

// TestResponsesStreamNormalizer_ReadSSEFrame: framing keeps event boundaries.
func TestResponsesStreamNormalizer_ReadSSEFrame(t *testing.T) {
	body := "event: a\ndata: {\"x\":1}\n\nevent: b\ndata: {\"y\":2}\n\n"
	br := bufio.NewReader(strings.NewReader(body))
	first, err := readSSEFrame(br)
	if err != nil || string(first) != "event: a\ndata: {\"x\":1}\n\n" {
		t.Fatalf("first frame = %q, err = %v", first, err)
	}
	second, err := readSSEFrame(br)
	if err != nil || string(second) != "event: b\ndata: {\"y\":2}\n\n" {
		t.Fatalf("second frame = %q, err = %v", second, err)
	}
	// EOF on a fresh frame boundary.
	third, err := readSSEFrame(br)
	if err != io.EOF || len(third) != 0 {
		t.Fatalf("expected clean EOF, got %q, err = %v", third, err)
	}
}

// --- translateResponsesToChat ------------------------------------------------

// TestTranslateResponsesToChat_SkipsEncryptedReasoning: a business-account
// response whose output[0] is an encrypted reasoning item but whose output[1]
// carries the plaintext answer must be translated, not rejected.
func TestTranslateResponsesToChat_SkipsEncryptedReasoning(t *testing.T) {
	body := `{"id":"resp_9","model":"gpt-5.6-luna","output":[` +
		`{"type":"reasoning","encrypted_content":"SGVsbG8...","summary":[]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi! How can I help you today?"}]}` +
		`],"usage":{"input_tokens":45,"output_tokens":37,"total_tokens":82}}`

	chatJSON, err := translateResponsesToChat(newFakeResp(200, body), "gpt-5.6-luna")
	if err != nil {
		t.Fatalf("expected plaintext message to be recovered, got error: %v", err)
	}
	var chat map[string]interface{}
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		t.Fatalf("translated body is not valid JSON: %v", err)
	}
	msg := chat["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hi! How can I help you today?" {
		t.Fatalf("content = %v, want the plaintext output_text", msg["content"])
	}
	if chat["model"] != "gpt-5.6-luna" {
		t.Fatalf("model = %v, want gpt-5.6-luna", chat["model"])
	}
	if !strings.HasPrefix(chat["id"].(string), "chatcmpl-") {
		t.Fatalf("id = %v, want chatcmpl- prefix", chat["id"])
	}
}

// TestTranslateResponsesToChat_FullyEncrypted: no plaintext anywhere -> error.
func TestTranslateResponsesToChat_FullyEncrypted(t *testing.T) {
	body := `{"id":"resp_9","model":"gpt-5.6-luna","output":[` +
		`{"type":"reasoning","encrypted_content":"SGVsbG8...","summary":[]}` +
		`],"usage":null}`
	if _, err := translateResponsesToChat(newFakeResp(200, body), "gpt-5.6-luna"); err == nil {
		t.Fatal("expected an error for a fully encrypted response")
	}
}

// TestTranslateResponsesToChat_MissingModelNoPanic: a response without a model
// field must fall back to the caller's model instead of panicking.
func TestTranslateResponsesToChat_MissingModelNoPanic(t *testing.T) {
	body := `{"id":"resp_9","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	chatJSON, err := translateResponsesToChat(newFakeResp(200, body), "fallback-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var chat map[string]interface{}
	_ = json.Unmarshal(chatJSON, &chat)
	if chat["model"] != "fallback-model" {
		t.Fatalf("model = %v, want fallback-model", chat["model"])
	}
}
