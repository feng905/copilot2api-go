package store

import (
	"encoding/json"
	"net/textproto"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Response header forwarding modes.
const (
	HeaderModePassthrough = "passthrough" // forward every upstream response header
	HeaderModeWhitelist   = "whitelist"   // forward only listed headers
	HeaderModeBlacklist   = "blacklist"   // forward everything except listed headers
)

// ResponseHeaderConfig is the persisted upstream response-header forwarding
// policy. Both lists are persisted regardless of mode so switching modes never
// loses editor state.
type ResponseHeaderConfig struct {
	Mode      string   `json:"mode"`
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
}

// HeaderPreset is a suggested header group offered as one-click options in the
// settings UI. Presets live here (single source of truth) and reach the UI via
// the GET settings endpoint.
type HeaderPreset struct {
	Group   string   `json:"group"` // "anthropic" | "openai" | "common"
	Headers []string `json:"headers"`
}

// Preset header names (lowercase; matching is case-insensitive so stored case
// is irrelevant).
var (
	presetAnthropicHeaders = []string{
		"request-id",
		"anthropic-version",
		"anthropic-ratelimit-requests-limit",
		"anthropic-ratelimit-requests-remaining",
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-limit",
		"anthropic-ratelimit-tokens-remaining",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-limit",
		"anthropic-ratelimit-input-tokens-remaining",
		"anthropic-ratelimit-input-tokens-reset",
	}
	presetOpenAIHeaders = []string{
		"x-request-id",
		"openai-processing-ms",
		"x-ratelimit-limit-requests",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-requests",
		"x-ratelimit-reset-tokens",
	}
	presetCommonHeaders = []string{
		"retry-after",
		"server",
		"date",
		"via",
	}
)

// defaultWhitelist ships as the out-of-the-box policy: the rate-limit and
// request-id headers clients actually use. Composed from the preset constants
// so preset and default never drift.
var defaultWhitelist = []string{
	presetCommonHeaders[0],    // retry-after
	presetAnthropicHeaders[0], // request-id
	presetOpenAIHeaders[0],    // x-request-id
	presetAnthropicHeaders[2], // anthropic-ratelimit-requests-limit
	presetAnthropicHeaders[3], // anthropic-ratelimit-requests-remaining
	presetAnthropicHeaders[4], // anthropic-ratelimit-requests-reset
	presetAnthropicHeaders[5], // anthropic-ratelimit-tokens-limit
	presetAnthropicHeaders[6], // anthropic-ratelimit-tokens-remaining
	presetAnthropicHeaders[7], // anthropic-ratelimit-tokens-reset
	presetOpenAIHeaders[2],    // x-ratelimit-limit-requests
	presetOpenAIHeaders[3],    // x-ratelimit-limit-tokens
	presetOpenAIHeaders[4],    // x-ratelimit-remaining-requests
	presetOpenAIHeaders[5],    // x-ratelimit-remaining-tokens
	presetOpenAIHeaders[6],    // x-ratelimit-reset-requests
	presetOpenAIHeaders[7],    // x-ratelimit-reset-tokens
}

var responseHeaderMu sync.RWMutex

// DefaultResponseHeaderConfig returns the shipped policy: whitelist mode with
// the common rate-limit headers preselected.
func DefaultResponseHeaderConfig() ResponseHeaderConfig {
	list := make([]string, len(defaultWhitelist))
	copy(list, defaultWhitelist)
	return ResponseHeaderConfig{
		Mode:      HeaderModeWhitelist,
		Whitelist: list,
		Blacklist: []string{},
	}
}

// ResponseHeaderPresets returns the preset header groups rendered as chips in
// the settings UI.
func ResponseHeaderPresets() []HeaderPreset {
	return []HeaderPreset{
		{Group: "anthropic", Headers: append([]string{}, presetAnthropicHeaders...)},
		{Group: "openai", Headers: append([]string{}, presetOpenAIHeaders...)},
		{Group: "common", Headers: append([]string{}, presetCommonHeaders...)},
	}
}

// headerNameRe matches an RFC 7230 header-name token: no spaces, no CR/LF, no
// colon. Used both to drop invalid names on normalize and to reject them on
// the update path.
var headerNameRe = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)

// IsHeaderTokenName reports whether name is a syntactically valid RFC 7230
// header-name token (non-empty, ≤128 chars, no spaces/CR/LF/colon). Exported
// for the console handler's update-path validation.
func IsHeaderTokenName(name string) bool {
	return name != "" && len(name) <= 128 && headerNameRe.MatchString(name)
}

// NormalizeResponseHeaderConfig canonicalizes list entries
// (textproto.CanonicalMIMEHeaderKey), drops empty/invalid names, dedupes
// case-insensitively, preserves order, and guarantees non-nil lists.
func NormalizeResponseHeaderConfig(cfg ResponseHeaderConfig) ResponseHeaderConfig {
	cfg.Mode = strings.TrimSpace(cfg.Mode)
	cfg.Whitelist = normalizeHeaderList(cfg.Whitelist)
	cfg.Blacklist = normalizeHeaderList(cfg.Blacklist)
	return cfg
}

func normalizeHeaderList(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, name := range list {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 128 || !headerNameRe.MatchString(name) {
			continue
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out
}

// GetResponseHeaderConfig reads the persisted policy. A missing, empty,
// unseeded ({}) or corrupt file yields the shipped default — the file starts
// as "{}" from EnsurePaths, so an empty Mode means "not configured yet".
// Per-response reads are fine (the file is ~300B; pool config is already read
// per request). If it ever matters, an atomic.Value snapshot refreshed on
// update is the drop-in optimization.
func GetResponseHeaderConfig() (ResponseHeaderConfig, error) {
	responseHeaderMu.RLock()
	defer responseHeaderMu.RUnlock()

	data, err := os.ReadFile(ResponseHeaderConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultResponseHeaderConfig(), nil
		}
		return DefaultResponseHeaderConfig(), nil
	}
	var cfg ResponseHeaderConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultResponseHeaderConfig(), nil
	}
	if cfg.Mode == "" {
		return DefaultResponseHeaderConfig(), nil
	}
	return NormalizeResponseHeaderConfig(cfg), nil
}

// UpdateResponseHeaderConfig validates nothing by itself beyond normalization —
// the caller (console handler) owns request validation — normalizes and
// persists the full policy.
func UpdateResponseHeaderConfig(cfg ResponseHeaderConfig) error {
	responseHeaderMu.Lock()
	defer responseHeaderMu.Unlock()

	cfg = NormalizeResponseHeaderConfig(cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ResponseHeaderConfigFile(), data, 0644)
}
