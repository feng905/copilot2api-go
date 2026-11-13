package anthropic

import "testing"

func TestTranslateToOpenAINormalizesAnthropicModelAndMetadata(t *testing.T) {
	payload := AnthropicMessagesPayload{
		Model:    "claude-sonnet-4-20250514",
		Messages: []AnthropicMessage{{Role: "user", Content: "hello"}},
		Metadata: &Metadata{UserID: "user-123"},
	}

	got := TranslateToOpenAI(payload)

	if got.Model != "claude-sonnet-4" {
		t.Fatalf("expected normalized model claude-sonnet-4, got %q", got.Model)
	}
	if got.User != "user-123" {
		t.Fatalf("expected user metadata to be forwarded, got %q", got.User)
	}
}

func TestTranslateToOpenAIMapsThinkingToReasoningEffort(t *testing.T) {
	payload := AnthropicMessagesPayload{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
		Thinking:  &ThinkingConfig{Type: "enabled", BudgetTokens: 8000},
	}

	got := TranslateToOpenAI(payload)

	if got.ReasoningEffort != "medium" {
		t.Fatalf("expected reasoning_effort medium for budget 8000, got %q", got.ReasoningEffort)
	}
}

func TestTranslateToOpenAIDisabledThinkingSetsNoReasoningEffort(t *testing.T) {
	payload := AnthropicMessagesPayload{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hello"}},
		Thinking:  &ThinkingConfig{Type: "disabled"},
	}

	got := TranslateToOpenAI(payload)

	if got.ReasoningEffort != "" {
		t.Fatalf("expected no reasoning_effort for disabled thinking, got %q", got.ReasoningEffort)
	}
}
