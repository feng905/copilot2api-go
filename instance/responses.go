package instance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"copilot-go/config"

	"github.com/gin-gonic/gin"
)

// DoResponsesProxy forwards /v1/responses requests through the unified router.
// It prefers the /responses endpoint when the model supports it, and falls back
// to /chat/completions (with translation back to Responses format) when the
// model is chat-only. This mirrors the policy used for /v1/chat/completions:
// the interface the USER called wins, translation only happens when needed.
func DoResponsesProxy(ctx context.Context, state *config.State, bodyBytes []byte) (*http.Response, error) {
	return routeCompletion(ctx, state, "responses", bodyBytes)
}

// ForwardResponsesResponse forwards the upstream response directly to client.
func ForwardResponsesResponse(c *gin.Context, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()

	contentType := resp.Header.Get("Content-Type")
	isStream := strings.Contains(contentType, "text/event-stream")

	if isStream {
		forwardResponsesStream(c, resp)
	} else {
		forwardResponsesNonStream(c, resp)
	}
}

// forwardResponsesStream relays the upstream SSE stream to the client frame by
// frame, normalizing ids on the way through (see responsesStreamNormalizer).
func forwardResponsesStream(c *gin.Context, resp *http.Response) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)

	reader := bufio.NewReaderSize(resp.Body, 10*1024*1024)
	normalizer := newResponsesStreamNormalizer()
	c.Stream(func(w io.Writer) bool {
		frame, err := readSSEFrame(reader)
		if len(frame) > 0 {
			if _, writeErr := w.Write(normalizer.processFrame(frame)); writeErr != nil {
				return false
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("Responses stream read error: %v", err)
			}
			return false
		}
		return true
	})
}

// readSSEFrame reads one SSE frame: every line up to and including the blank
// line that terminates the event, or whatever remains at EOF.
func readSSEFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			frame = append(frame, line...)
			if isBlankSSELine(line) {
				return frame, nil
			}
		}
		if err != nil {
			return frame, err
		}
	}
}

func isBlankSSELine(line []byte) bool {
	return bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
}

// responsesStreamNormalizer rewrites the rotating encrypted ids emitted by the
// GitHub Copilot /responses stream. On some (business/enterprise) accounts every
// SSE event carries a freshly encrypted id for the same logical item, which
// breaks spec-compliant clients that correlate events by item id — e.g. the
// Vercel AI SDK used by Cherry Studio crashes with "Cannot read properties of
// undefined (reading 'summaryParts')" when the output_item.done id does not
// match the output_item.added id. The normalizer pins each output item to the
// id from its output_item.added event and rewrites every later reference
// (item_id on progress events, output_item.done, full-response snapshots) to
// it; the top-level response id is pinned by response.created. For upstreams
// that already emit consistent ids every rewrite is an identity operation and
// frames pass through byte-for-byte.
type responsesStreamNormalizer struct {
	itemID     map[int]string // output_index -> canonical item id
	responseID string         // canonical response id (from response.created)
}

func newResponsesStreamNormalizer() *responsesStreamNormalizer {
	return &responsesStreamNormalizer{itemID: make(map[int]string)}
}

// processFrame normalizes one complete SSE frame (terminated by a blank line,
// or the final partial frame at EOF). Frames that need no change are returned
// byte-for-byte.
func (n *responsesStreamNormalizer) processFrame(frame []byte) []byte {
	payload, prefix, dataLine, ok := singleSSEDataLine(frame)
	if !ok {
		return frame
	}

	var ev map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber() // keep number literals byte-stable across the round trip
	if err := dec.Decode(&ev); err != nil {
		return frame
	}
	if !n.normalizeEvent(ev) {
		return frame
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return frame
	}

	// Re-emit the frame with the normalized payload, preserving the original
	// event lines and the data line's terminator style.
	lines := sseFrameLines(frame)
	rebuilt := make([]byte, 0, len(frame)+len(out))
	for i, line := range lines {
		if i != dataLine {
			rebuilt = append(rebuilt, line...)
			continue
		}
		rebuilt = append(rebuilt, prefix...)
		rebuilt = append(rebuilt, out...)
		rebuilt = append(rebuilt, lineTerminator(line)...)
	}
	return rebuilt
}

// normalizeEvent mutates ev in place, pinning ids to their canonical values.
// It reports whether anything changed.
func (n *responsesStreamNormalizer) normalizeEvent(ev map[string]interface{}) bool {
	t, _ := ev["type"].(string)
	changed := false

	// Top-level response id: response.created establishes the canonical id,
	// every later response snapshot is rewritten to it.
	if t == "response.created" {
		if r, ok := ev["response"].(map[string]interface{}); ok {
			if id, ok := r["id"].(string); ok && id != "" {
				n.responseID = id
			}
		}
	} else if r, ok := ev["response"].(map[string]interface{}); ok {
		if n.responseID != "" {
			if id, _ := r["id"].(string); id != n.responseID {
				r["id"] = n.responseID
				changed = true
			}
		}
		if n.rewriteOutputIDs(r["output"]) {
			changed = true
		}
	}

	switch t {
	case "response.output_item.added":
		if idx, ok := jsonInt(ev["output_index"]); ok {
			if item, ok := ev["item"].(map[string]interface{}); ok {
				if id, ok := item["id"].(string); ok && id != "" {
					n.itemID[idx] = id
				}
			}
		}
	case "response.output_item.done":
		if idx, ok := jsonInt(ev["output_index"]); ok {
			if item, ok := ev["item"].(map[string]interface{}); ok {
				if canonical, ok := n.itemID[idx]; ok {
					if cur, _ := item["id"].(string); cur != canonical {
						item["id"] = canonical
						changed = true
					}
				}
			}
		}
	default:
		// Item-scoped progress events (content_part.*, output_text.*,
		// reasoning_summary_*, function_call_arguments.*, ...) carry both
		// item_id and output_index.
		if idx, ok := jsonInt(ev["output_index"]); ok {
			if canonical, ok := n.itemID[idx]; ok {
				if cur, _ := ev["item_id"].(string); cur != canonical {
					ev["item_id"] = canonical
					changed = true
				}
			}
		}
	}
	return changed
}

// rewriteOutputIDs pins the ids inside a full-response snapshot (e.g.
// response.completed). The output array is ordered by output_index, so the
// array position identifies the item.
func (n *responsesStreamNormalizer) rewriteOutputIDs(output interface{}) bool {
	arr, ok := output.([]interface{})
	if !ok {
		return false
	}
	changed := false
	for i, raw := range arr {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if canonical, ok := n.itemID[i]; ok {
			if cur, _ := m["id"].(string); cur != canonical {
				m["id"] = canonical
				changed = true
			}
		}
	}
	return changed
}

// sseFrameLines splits a frame into lines, each including its terminator.
func sseFrameLines(frame []byte) [][]byte {
	var lines [][]byte
	for len(frame) > 0 {
		i := bytes.IndexByte(frame, '\n')
		if i < 0 {
			lines = append(lines, frame)
			break
		}
		lines = append(lines, frame[:i+1])
		frame = frame[i+1:]
	}
	return lines
}

func lineTerminator(line []byte) []byte {
	body := bytes.TrimSuffix(line, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	return line[len(body):]
}

// singleSSEDataLine locates the frame's data payload. Only frames with exactly
// one data line are rewritten; anything else passes through untouched. It
// returns the payload, the original "data:"-prefix (including any space) and
// the index of the data line among the frame's lines.
func singleSSEDataLine(frame []byte) (payload []byte, prefix []byte, lineIdx int, ok bool) {
	dataLines := 0
	for i, line := range sseFrameLines(frame) {
		body := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if !bytes.HasPrefix(body, []byte("data:")) {
			continue
		}
		dataLines++
		payload = bytes.TrimPrefix(body[5:], []byte(" "))
		prefix = body[:5]
		if len(body) > 5 && body[5] == ' ' {
			prefix = body[:6]
		}
		lineIdx = i
	}
	return payload, prefix, lineIdx, dataLines == 1
}

// jsonInt extracts an integer from a decoded JSON number, tolerating both
// json.Number (UseNumber) and float64 decoding.
func jsonInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		i := int(n)
		return i, n == float64(i)
	}
	return 0, false
}

func forwardResponsesNonStream(c *gin.Context, resp *http.Response) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	// Try to filter out empty reasoning items for better client compatibility
	var respData map[string]interface{}
	if err := json.Unmarshal(body, &respData); err == nil {
		if output, ok := respData["output"].([]interface{}); ok {
			filtered := make([]interface{}, 0)
			for _, item := range output {
				if m, ok := item.(map[string]interface{}); ok {
					// Skip empty reasoning items (Copilot doesn't expose reasoning content)
					if itemType, _ := m["type"].(string); itemType == "reasoning" {
						if summary, _ := m["summary"].([]interface{}); len(summary) == 0 {
							continue // Skip empty reasoning
						}
					}
					filtered = append(filtered, item)
				}
			}
			respData["output"] = filtered
			if filteredBody, err := json.Marshal(respData); err == nil {
				body = filteredBody
			}
		}
	}

	c.Data(resp.StatusCode, "application/json", body)
}
