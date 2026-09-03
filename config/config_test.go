package config

import "testing"

// TestCopilotRestBaseURL verifies the REST base URL is taken from the token
// endpoint's reported endpoints.api, falling back to the account-type heuristic
// only when that field is absent. This guarantees model calls always hit the
// host GitHub authorized for the token (e.g. api.business.githubcopilot.com for
// business seats) instead of the host implied by the stored accountType.
func TestCopilotRestBaseURL(t *testing.T) {
	business := "https://api.business.githubcopilot.com"
	individual := "https://api.githubcopilot.com"

	// Token endpoint reports business host -> wins regardless of accountType.
	st := &State{RestEndpoint: business, AccountType: "individual"}
	if got := CopilotRestBaseURL(st); got != business {
		t.Errorf("RestEndpoint should take precedence: got %q want %q", got, business)
	}

	// Token endpoint reports individual host -> used as-is.
	st = &State{RestEndpoint: individual, AccountType: "business"}
	if got := CopilotRestBaseURL(st); got != individual {
		t.Errorf("RestEndpoint should take precedence even for business accountType: got %q want %q", got, individual)
	}

	// No RestEndpoint -> fall back to account-type heuristic.
	st = &State{RestEndpoint: "", AccountType: "individual"}
	if got := CopilotRestBaseURL(st); got != CopilotIndividualChatURL {
		t.Errorf("empty RestEndpoint should fall back to individual URL: got %q want %q", got, CopilotIndividualChatURL)
	}

	st = &State{RestEndpoint: "", AccountType: "business"}
	if got := CopilotRestBaseURL(st); got != "https://api.business.githubcopilot.com" {
		t.Errorf("empty RestEndpoint should fall back to business URL: got %q", got)
	}

	// Nil state -> individual default (safe).
	if got := CopilotRestBaseURL(nil); got != CopilotIndividualChatURL {
		t.Errorf("nil state should default to individual URL: got %q", got)
	}
}
