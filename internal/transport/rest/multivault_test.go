package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// ── POST /api/activate — vaults[]/weights[] merged recall ──────────────────

func TestActivate_MultiVault_MergesAndReturnsDegraded(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory"],"vaults":["proj-a","agent-memory"],"vault_weights":[0.67,0.33],"max_results":10}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	acts, _ := resp["activations"].([]any)
	if len(acts) != 2 {
		t.Fatalf("expected 2 activations (1 per vault, from MockEngine.Activate), got %d: %v", len(acts), resp)
	}
}

func TestActivate_MultiVault_VaultAndVaultsMutuallyExclusive(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory"],"vault":"default","vaults":["proj-a","agent-memory"]}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for vault+vaults mutual exclusivity, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_MultiVault_WeightsLengthMismatch(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory"],"vaults":["proj-a","agent-memory"],"vault_weights":[1.0]}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for weights/vaults length mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_MultiVault_ScopeViolationRejectedNoLeak(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory"],"vaults":["agent-memory","someone-elses-vault"]}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "agent-memory") || strings.Contains(w.Body.String(), "proj-*") {
		t.Errorf("error response leaked scope contents: %s", w.Body.String())
	}
}

func TestActivate_MultiVault_AllInScopeSucceeds(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory"],"vaults":["agent-memory","proj-muninndb-3f2a1b9c"]}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ── POST /api/vaults — scoped-key provisioning auth matrix ─────────────────

func TestCreateVault_FullModeInScope_Created(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"proj-newthing-abcd1234","template":"knowledge-graph"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp CreateVaultResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Created {
		t.Error("expected Created=true for first provision")
	}
	if resp.Vault.Plasticity == nil || resp.Vault.Plasticity.Preset != "knowledge-graph" {
		t.Errorf("expected knowledge-graph preset persisted, got %+v", resp.Vault.Plasticity)
	}
}

func TestCreateVault_Idempotent_DoesNotClobberExistingConfig(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"proj-idempotent-abcd1234","template":"knowledge-graph","retention_days":90}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)
		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200/201, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// Second call with a DIFFERENT template must not clobber the first config.
	body2 := `{"name":"proj-idempotent-abcd1234","template":"reference"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body2))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent no-op), got %d: %s", w.Code, w.Body.String())
	}
	var resp CreateVaultResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Created {
		t.Error("expected Created=false on second call to an already-provisioned vault")
	}
	if resp.Vault.Plasticity == nil || resp.Vault.Plasticity.Preset != "knowledge-graph" {
		t.Errorf("expected original knowledge-graph preset preserved, got %+v — config was clobbered", resp.Vault.Plasticity)
	}
}

func TestCreateVault_ObserveModeKeyForbidden(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeObserve, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"proj-forbidden-abcd1234"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for observe-mode key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateVault_WriteModeKeyForbidden(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeWrite, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"proj-forbidden2-abcd1234"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for write-mode key, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateVault_OutOfScopeNameRejectedNoLeak(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"someone-elses-vault"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for out-of-scope vault name, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "agent-memory") || strings.Contains(w.Body.String(), "proj-*") {
		t.Errorf("error response leaked scope contents: %s", w.Body.String())
	}
}

func TestCreateVault_NoAPIKey_UnauthorizedFailsClosed(t *testing.T) {
	store := newTestAuthStore(t)
	server := newTestServer(t, store)

	body := `{"name":"proj-noauth-abcd1234"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Fatalf("expected auth failure for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateVault_InvalidTemplateRejected(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"proj-badtemplate-abcd1234","template":"not-a-real-preset"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid template, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateVault_InvalidNameRejected(t *testing.T) {
	store := newTestAuthStore(t)
	token, _, err := store.GenerateScopedAPIKey([]string{"agent-memory", "proj-*"}, "test-key", auth.ModeFull, nil, false)
	if err != nil {
		t.Fatalf("GenerateScopedAPIKey: %v", err)
	}
	server := newTestServer(t, store)

	body := `{"name":"Not A Valid Name!"}`
	req := httptest.NewRequest("POST", "/api/vaults", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid vault name, got %d: %s", w.Code, w.Body.String())
	}
}
