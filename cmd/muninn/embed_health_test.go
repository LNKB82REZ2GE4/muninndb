package main

import (
	"context"
	"testing"
	"time"

	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
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

// TestBuildEmbedder_FailedEnvProviderNotSubstitutedByLocal pins the #582
// principle for env-configured providers (adversarial-review finding D2):
// when the requested provider fails at startup, the bundled local model must
// not be silently substituted (and then reported healthy).
func TestBuildEmbedder_FailedEnvProviderNotSubstitutedByLocal(t *testing.T) {
	if !embedpkg.LocalAvailable() {
		t.Skip("bundled local model not built in (needs -tags localassets)")
	}
	clearEmbedProviderEnv(t)
	t.Setenv("MUNINN_LOCAL_EMBED", "")
	t.Setenv("MUNINN_OLLAMA_URL", "ollama://127.0.0.1:1/nomic-embed-text")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, embedPlugin, err := buildEmbedder(ctx, plugincfg.PluginConfig{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildEmbedder: %v", err)
	}
	if embedPlugin != nil {
		t.Fatalf("a failed env provider was substituted by %q", embedPlugin.Name())
	}
	if s := newEmbedHealth(plugincfg.PluginConfig{}, embedPlugin).Snapshot(); s.State != plugin.EmbedHealthDisabled {
		t.Fatalf("state = %q, want disabled", s.State)
	}
}
