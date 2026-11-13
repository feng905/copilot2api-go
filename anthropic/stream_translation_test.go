package anthropic

import "testing"

func intPtr(v int) *int { return &v }

// Text deltas arriving after a tool_use block must share one new text block,
// not open a fresh block per delta.
func TestTextAfterToolCallSharesOneBlock(t *testing.T) {
	state := NewStreamState()
	feed := func(delta *ChoiceMsg) []StreamEvent {
		return TranslateChunkToAnthropicEvents(ChatCompletionResponse{
			ID: "x", Model: "m",
			Choices: []Choice{{Delta: delta}},
		}, state)
	}

	feed(&ChoiceMsg{Content: "before "})
	feed(&ChoiceMsg{ToolCalls: []ToolCall{{
		ID: "call_0", Type: "function", Index: intPtr(0),
		Function: FunctionCall{Name: "f", Arguments: `{"a":1}`},
	}}})
	feed(&ChoiceMsg{Content: "after "})
	extra := feed(&ChoiceMsg{Content: "more"})

	for _, ev := range extra {
		if ev.Event == "content_block_start" {
			t.Fatalf("second post-tool text delta opened a new block: %#v", ev.Data)
		}
	}
	if !state.ContentBlockOpen || state.OpenBlockIsTool {
		t.Fatalf("expected an open text block, state: %+v", state)
	}
	// Blocks so far: text(0), tool_use(1), text(2).
	if state.ContentBlockIndex != 2 {
		t.Fatalf("expected current block index 2, got %d", state.ContentBlockIndex)
	}
}

func TestTranslateChunkToAnthropicEventsCarriesCachedUsage(t *testing.T) {
	finishReason := "stop"
	events := TranslateChunkToAnthropicEvents(ChatCompletionResponse{
		ID:    "msg_1",
		Model: "claude-sonnet-4",
		Usage: &OpenAIUsage{
			PromptTokens:     100,
			CompletionTokens: 22,
			PromptTokensDetails: &PromptTokensDetails{
				CachedTokens: 40,
			},
		},
		Choices: []Choice{{
			Delta:        &ChoiceMsg{Content: "hello"},
			FinishReason: &finishReason,
		}},
	}, NewStreamState())

	messageStart, ok := events[0].Data.(MessageStartEvent)
	if !ok {
		t.Fatalf("expected first event to be MessageStartEvent, got %#v", events[0].Data)
	}
	if messageStart.Message.Usage.InputTokens != 60 {
		t.Fatalf("expected message_start input tokens to exclude cached tokens, got %d", messageStart.Message.Usage.InputTokens)
	}
	if messageStart.Message.Usage.CacheReadInputTokens != 40 {
		t.Fatalf("expected message_start cached token count, got %d", messageStart.Message.Usage.CacheReadInputTokens)
	}

	var deltaUsage *DeltaUsage
	for _, event := range events {
		if messageDelta, ok := event.Data.(MessageDeltaEvent); ok {
			deltaUsage = messageDelta.Usage
			break
		}
	}
	if deltaUsage == nil {
		t.Fatal("expected a message_delta event with usage")
	}
	if deltaUsage.InputTokens != 60 || deltaUsage.OutputTokens != 22 || deltaUsage.CacheReadInputTokens != 40 {
		t.Fatalf("unexpected message_delta usage: %+v", *deltaUsage)
	}
}

func TestStreamReasoningContentEmitsThinkingBlock(t *testing.T) {
	events := TranslateChunkToAnthropicEvents(ChatCompletionResponse{
		ID:    "chatcmpl-1",
		Model: "claude-sonnet-4",
		Choices: []Choice{{
			Index: 0,
			Delta: &ChoiceMsg{ReasoningContent: "Let me think..."},
		}},
	}, NewStreamState())

	// message_start, ping, content_block_start(thinking), content_block_delta(thinking_delta)
	if len(events) < 4 {
		t.Fatalf("expected at least 4 events, got %d: %#v", len(events), events)
	}
	blockStart, ok := events[2].Data.(ContentBlockStartEvent)
	if !ok {
		t.Fatalf("expected ContentBlockStartEvent, got %#v", events[2].Data)
	}
	if blockStart.ContentBlock.Type != "thinking" {
		t.Fatalf("expected thinking block, got %q", blockStart.ContentBlock.Type)
	}
	delta, ok := events[3].Data.(ContentBlockDeltaEvent)
	if !ok {
		t.Fatalf("expected ContentBlockDeltaEvent, got %#v", events[3].Data)
	}
	if delta.Delta.Type != "thinking_delta" || delta.Delta.ThinkingText != "Let me think..." {
		t.Fatalf("unexpected thinking delta: %+v", delta.Delta)
	}
}

func TestStreamReasoningThenTextSplitsBlocks(t *testing.T) {
	state := NewStreamState()
	TranslateChunkToAnthropicEvents(ChatCompletionResponse{
		Choices: []Choice{{Index: 0, Delta: &ChoiceMsg{ReasoningContent: "thinking part"}}},
	}, state)
	events := TranslateChunkToAnthropicEvents(ChatCompletionResponse{
		Choices: []Choice{{Index: 0, Delta: &ChoiceMsg{Content: "answer"}}},
	}, state)

	// First event should close the thinking block, then start a text block.
	if len(events) < 2 {
		t.Fatalf("expected block transition events, got %#v", events)
	}
	stop, ok := events[0].Data.(ContentBlockStopEvent)
	if !ok || stop.Index != 0 {
		t.Fatalf("expected content_block_stop for thinking block, got %#v", events[0].Data)
	}
	start, ok := events[1].Data.(ContentBlockStartEvent)
	if !ok || start.ContentBlock.Type != "text" || start.Index != 1 {
		t.Fatalf("expected text block at index 1, got %#v", events[1].Data)
	}
}

func TestTranslateToAnthropicReasoningContentBecomesThinkingBlock(t *testing.T) {
	resp := ChatCompletionResponse{
		ID:    "chatcmpl-1",
		Model: "claude-sonnet-4",
		Choices: []Choice{{
			Index: 0,
			Message: &ChoiceMsg{
				ReasoningContent: "step by step",
				Content:          "answer",
			},
		}},
	}

	got := TranslateToAnthropic(resp)

	if len(got.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d: %#v", len(got.Content), got.Content)
	}
	if got.Content[0].Type != "thinking" || got.Content[0].Thinking != "step by step" {
		t.Fatalf("expected thinking block first, got %#v", got.Content[0])
	}
	if got.Content[1].Type != "text" || got.Content[1].Text != "answer" {
		t.Fatalf("expected text block second, got %#v", got.Content[1])
	}
}
