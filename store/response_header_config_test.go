package store

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempAppDir redirects the store's data directory to a fresh temp dir for
// the duration of the test. Do NOT run tests that use this with t.Parallel.
func withTempAppDir(t *testing.T) {
	t.Helper()
	old := AppDir
	AppDir = t.TempDir()
	t.Cleanup(func() { AppDir = old })
}

func TestGetResponseHeaderConfigDefaults(t *testing.T) {
	withTempAppDir(t)

	// Missing file, empty file, {} seed and corrupt JSON all fall back to the
	// shipped default (whitelist + common headers).
	cases := map[string]func() error{
		"missing": func() error { return nil },
		"empty": func() error {
			return os.WriteFile(ResponseHeaderConfigFile(), nil, 0644)
		},
		"empty object": func() error {
			return os.WriteFile(ResponseHeaderConfigFile(), []byte("{}"), 0644)
		},
		"corrupt": func() error {
			return os.WriteFile(ResponseHeaderConfigFile(), []byte("not json"), 0644)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			if err := seed(); err != nil {
				t.Fatal(err)
			}
			cfg, err := GetResponseHeaderConfig()
			if err != nil {
				t.Fatalf("GetResponseHeaderConfig: %v", err)
			}
			def := DefaultResponseHeaderConfig()
			if cfg.Mode != def.Mode || len(cfg.Whitelist) != len(def.Whitelist) || len(cfg.Blacklist) != 0 {
				t.Fatalf("expected default config, got %+v", cfg)
			}
			if len(cfg.Whitelist) == 0 {
				t.Fatal("default whitelist is empty")
			}
			for i, h := range cfg.Whitelist {
				if h != def.Whitelist[i] {
					t.Fatalf("whitelist drift at %d: got %s want %s", i, h, def.Whitelist[i])
				}
			}
		})
	}
}

func TestGetResponseHeaderConfigExplicitModePreserved(t *testing.T) {
	withTempAppDir(t)

	if err := os.MkdirAll(AppDir, 0755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"mode":"passthrough","whitelist":["x-request-id"],"blacklist":[]}`)
	if err := os.WriteFile(ResponseHeaderConfigFile(), data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := GetResponseHeaderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != HeaderModePassthrough {
		t.Fatalf("expected passthrough preserved, got %q", cfg.Mode)
	}
	if len(cfg.Whitelist) != 1 || cfg.Whitelist[0] != "X-Request-Id" {
		t.Fatalf("unexpected whitelist: %+v", cfg.Whitelist)
	}
}

func TestUpdateResponseHeaderConfigRoundTrip(t *testing.T) {
	withTempAppDir(t)

	in := ResponseHeaderConfig{
		Mode:      HeaderModeBlacklist,
		Whitelist: []string{"retry-after"},
		Blacklist: []string{"x-custom", "X-CUSTOM", "bad name", "", "X-Request-Id"},
	}
	if err := UpdateResponseHeaderConfig(in); err != nil {
		t.Fatal(err)
	}

	cfg, err := GetResponseHeaderConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != HeaderModeBlacklist {
		t.Fatalf("mode lost: %q", cfg.Mode)
	}
	if len(cfg.Blacklist) != 2 || cfg.Blacklist[0] != "X-Custom" || cfg.Blacklist[1] != "X-Request-Id" {
		t.Fatalf("blacklist not normalized (dupes/invalid should be dropped, order kept): %+v", cfg.Blacklist)
	}
	if len(cfg.Whitelist) != 1 || cfg.Whitelist[0] != "Retry-After" {
		t.Fatalf("whitelist not canonicalized: %+v", cfg.Whitelist)
	}
}

func TestNormalizeResponseHeaderConfig(t *testing.T) {
	got := NormalizeResponseHeaderConfig(ResponseHeaderConfig{
		Mode:      " whitelist ",
		Whitelist: []string{"x-request-id", "X-Request-Id", "", "bad name", "Retry-After"},
		Blacklist: nil,
	})
	if got.Mode != "whitelist" {
		t.Fatalf("mode not trimmed: %q", got.Mode)
	}
	if len(got.Whitelist) != 2 || got.Whitelist[0] != "X-Request-Id" || got.Whitelist[1] != "Retry-After" {
		t.Fatalf("unexpected whitelist: %+v", got.Whitelist)
	}
	if got.Blacklist == nil || len(got.Blacklist) != 0 {
		t.Fatalf("nil blacklist should normalize to empty slice, got %+v", got.Blacklist)
	}
}

func TestIsHeaderTokenName(t *testing.T) {
	valid := []string{"x-request-id", "X-Request-Id", "retry-after", "a", "X_Custom~Header!"}
	invalid := []string{"", "bad name", "with:colon", "cr{lf}", "a b", string(make([]byte, 200))}
	for _, name := range valid {
		if !IsHeaderTokenName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	for _, name := range invalid {
		if IsHeaderTokenName(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestResponseHeaderConfigFileUnderAppDir(t *testing.T) {
	withTempAppDir(t)
	if want := filepath.Join(AppDir, "response-header-config.json"); ResponseHeaderConfigFile() != want {
		t.Fatalf("unexpected file path: %s", ResponseHeaderConfigFile())
	}
}
