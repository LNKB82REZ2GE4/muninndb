package main

import (
	"testing"

	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
)

func clearEmbedProviderEnv(t *testing.T) {
	t.Helper()
	for _, v := range embedEnvProviderVars {
		t.Setenv(v, "")
	}
}

// TestNewEmbedHealth_EnvProviderFailureIsDisabled pins G4: a provider
// requested via the environment that failed to initialize (buildEmbedder
// returned no plugin) is "disabled" — an alert-worthy state — not
// "unconfigured". This is the wrong-API-key-at-startup case.
func TestNewEmbedHealth_EnvProviderFailureIsDisabled(t *testing.T) {
	clearEmbedProviderEnv(t)
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
	t.Setenv("MUNINN_OPENAI_KEY", "wrong-key")
	s := newEmbedHealth(plugincfg.PluginConfig{}, nil).Snapshot()
	if s.State != plugin.EmbedHealthDisabled || !s.Degraded() {
		t.Fatalf("state = %q, want disabled", s.State)
	}
}

func TestNewEmbedHealth_NothingRequestedIsUnconfigured(t *testing.T) {
	clearEmbedProviderEnv(t)
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
	s := newEmbedHealth(plugincfg.PluginConfig{}, nil).Snapshot()
	if s.State != plugin.EmbedHealthUnconfigured || s.Degraded() {
		t.Fatalf("state = %q, want unconfigured (not degraded)", s.State)
	}
}

func TestNewEmbedHealth_SavedProviderFailureIsDisabled(t *testing.T) {
	clearEmbedProviderEnv(t)
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
	s := newEmbedHealth(plugincfg.PluginConfig{EmbedProvider: "ollama"}, nil).Snapshot()
	if s.State != plugin.EmbedHealthDisabled {
		t.Fatalf("state = %q, want disabled", s.State)
	}
}
