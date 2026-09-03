package config

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	CopilotVersion   = "0.26.7"
	GithubClientID   = "Iv1.b507a08c87ecfe98"
	GithubAPIVersion = "2025-04-01"

	CopilotIndividualChatURL = "https://api.githubcopilot.com"

	// CopilotBusinessChatURL is the native Copilot REST API host. It accepts
	// the raw GitHub token (not the exchanged Copilot token) together with the
	// official Copilot CLI header set, and exposes /chat/completions,
	// /responses and /models as OpenAI-compatible REST endpoints. This is the
	// host verified to work for direct proxying (see instance/rest_completions.go).
	CopilotBusinessChatURL = "https://api.business.githubcopilot.com"

	GithubCopilotURL = "https://api.github.com/copilot_internal/v2/token"
	GithubDeviceURL  = "https://github.com/login/device/code"
	GithubTokenURL   = "https://github.com/login/oauth/access_token"
	GithubUserURL    = "https://api.github.com/user"
)

// proxyURL is the global outbound HTTP proxy. Protected by proxyMu.
var (
	proxyMu  sync.RWMutex
	proxyURL string
)

func SetProxyURL(u string) {
	proxyMu.Lock()
	proxyURL = u
	proxyMu.Unlock()
}

func GetProxyURL() string {
	proxyMu.RLock()
	u := proxyURL
	proxyMu.RUnlock()
	return u
}

// GetSystemPrompt returns the server-configured system prompt (set via the
// COPILOT_SYSTEM_PROMPT env var). It is injected into Copilot sessions through
// OrganizationCustomInstructions so the caller's own request body is never
// modified. Empty when unset.
func GetSystemPrompt() string {
	return os.Getenv("COPILOT_SYSTEM_PROMPT")
}

// NewHTTPClient creates an HTTP client with the current proxy setting and given timeout.
func NewHTTPClient(timeout time.Duration) *http.Client {
	t := &http.Transport{
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if pURL := GetProxyURL(); pURL != "" {
		if parsed, err := url.Parse(pURL); err == nil {
			t.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Timeout: timeout, Transport: t}
}

type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

type ModelCapabilities struct {
	Family string      `json:"family,omitempty"`
	Limits ModelLimits `json:"limits,omitempty"`
	Type   string      `json:"type,omitempty"`
}

type ModelLimits struct {
	MaxOutputTokens  int `json:"max_output_tokens,omitempty"`
	MaxPromptTokens  int `json:"max_prompt_tokens,omitempty"`
	MaxContextWindow int `json:"max_context_window,omitempty"`
}

type ModelEntry struct {
	ID           string             `json:"id"`
	Object       string             `json:"object"`
	Created      int64              `json:"created"`
	OwnedBy      string             `json:"owned_by"`
	Name         string             `json:"name,omitempty"`
	Version      string             `json:"version,omitempty"`
	Vendor       string             `json:"vendor,omitempty"`
	Capabilities *ModelCapabilities `json:"capabilities,omitempty"`
	// SupportedEndpoints lists the REST endpoints a model can be called on,
	// e.g. ["/chat/completions"] or ["/responses","ws:/responses"]. Used to
	// route OpenAI-style requests to the correct native endpoint.
	SupportedEndpoints []string `json:"supported_endpoints,omitempty"`
}

type CopilotTokenResponse struct {
	Token     string         `json:"token"`
	ExpiresAt int64          `json:"expires_at"`
	Endpoints TokenEndpoints `json:"endpoints"`
}

// TokenEndpoints carries the hostnames the token endpoint authorizes for this
// token. endpoints.api is the native Copilot REST base URL (e.g.
// https://api.business.githubcopilot.com for business seats) and is the
// authoritative host to call for /chat/completions, /responses and /models.
type TokenEndpoints struct {
	API string `json:"api"`
}

type State struct {
	mu             sync.RWMutex
	GithubToken    string
	CopilotToken   string
	TokenExpiresAt int64 // Unix timestamp when the Copilot token expires
	AccountType    string
	// RestEndpoint is the native Copilot REST base URL reported by the token
	// endpoint (api.github.com/copilot_internal/v2/token -> endpoints.api). It
	// takes precedence over the account-type-derived URL so model calls always
	// hit the host GitHub actually authorized for this token. Empty until the
	// first token refresh populates it.
	RestEndpoint string
	Models       *ModelsResponse
	VSCodeVersion string
}

func NewState() *State {
	return &State{}
}

func (s *State) Lock()    { s.mu.Lock() }
func (s *State) Unlock()  { s.mu.Unlock() }
func (s *State) RLock()   { s.mu.RLock() }
func (s *State) RUnlock() { s.mu.RUnlock() }

func CopilotBaseURL(accountType string) string {
	if accountType == "" || accountType == "individual" {
		return CopilotIndividualChatURL
	}
	return fmt.Sprintf("https://api.%s.githubcopilot.com", accountType)
}

// CopilotRestBaseURL returns the base URL to use for native Copilot REST calls
// (/chat/completions, /responses, /models). It prefers the endpoints.api value
// returned by the token endpoint, which is the authoritative host GitHub
// authorizes for this token (e.g. https://api.business.githubcopilot.com for
// business seats, https://api.githubcopilot.com for individual seats). It falls
// back to the account-type heuristic only when the token endpoint did not
// report one.
func CopilotRestBaseURL(state *State) string {
	if state != nil && state.RestEndpoint != "" {
		return state.RestEndpoint
	}
	if state != nil {
		return CopilotBaseURL(state.AccountType)
	}
	return CopilotIndividualChatURL
}

func CopilotHeaders(state *State, vision bool) http.Header {
	state.RLock()
	defer state.RUnlock()

	h := make(http.Header)
	h.Set("Authorization", "Bearer "+state.CopilotToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Copilot-Integration-Id", "vscode-chat")
	h.Set("Editor-Version", "vscode/"+state.VSCodeVersion)
	h.Set("Editor-Plugin-Version", "copilot-chat/"+CopilotVersion)
	h.Set("User-Agent", fmt.Sprintf("GitHubCopilotChat/%s", CopilotVersion))
	h.Set("Openai-Intent", "conversation-panel")
	h.Set("X-GitHub-API-Version", GithubAPIVersion)
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("X-Vscode-User-Agent-Library-Version", "electron-fetch")
	if vision {
		h.Set("Copilot-Vision-Request", "true")
	}
	return h
}

func GithubHeaders(state *State) http.Header {
	state.RLock()
	defer state.RUnlock()

	h := make(http.Header)
	h.Set("Authorization", "token "+state.GithubToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Editor-Version", "vscode/"+state.VSCodeVersion)
	h.Set("Editor-Plugin-Version", "copilot-chat/"+CopilotVersion)
	h.Set("User-Agent", fmt.Sprintf("GitHubCopilotChat/%s", CopilotVersion))
	h.Set("X-GitHub-API-Version", GithubAPIVersion)
	h.Set("X-Vscode-User-Agent-Library-Version", "electron-fetch")
	return h
}

// CopilotRestHeaders builds the header set required to call the native Copilot
// REST API (chat/completions, responses, models) directly. This is the exact
// header set the official GitHub Copilot CLI uses, and it authenticates with
// the raw GitHub token (state.GithubToken) — verified to work against
// api.business.githubcopilot.com. Using these headers (instead of the exchanged
// Copilot token + vscode-chat integration id) is what makes direct REST
// proxying succeed.
func CopilotRestHeaders(state *State) http.Header {
	state.RLock()
	defer state.RUnlock()

	h := make(http.Header)
	h.Set("Authorization", "Bearer "+state.GithubToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Copilot-Integration-Id", "copilot-developer-cli")
	h.Set("Copilot-Harness-Id", "copilot-sdk")
	h.Set("Editor-Version", "copilot/1.0.82")
	h.Set("X-GitHub-Api-Version", "2026-08-01")
	h.Set("X-Initiator", "user")
	h.Set("User-Agent", "copilot/1.0.82")
	return h
}
