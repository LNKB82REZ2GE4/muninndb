package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── resolveVaultsWeighted unit tests ────────────────────────────────────────

func TestResolveVaultsWeighted_AbsentVaultsFallsBackToSingleVault(t *testing.T) {
	vaults, weights, present, errMsg := resolveVaultsWeighted(nil, map[string]any{"vault": "default"})
	if present {
		t.Fatalf("present = true, want false (no 'vaults' key)")
	}
	if errMsg != "" || vaults != nil || weights != nil {
		t.Errorf("expected zero values on absent 'vaults', got vaults=%v weights=%v err=%q", vaults, weights, errMsg)
	}
}

func TestResolveVaultsWeighted_MutuallyExclusiveWithVault(t *testing.T) {
	_, _, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vault":  "a",
		"vaults": []any{"a", "b"},
	})
	if !present {
		t.Fatal("present should be true once 'vaults' key is seen, even on error")
	}
	if !strings.Contains(errMsg, "mutually exclusive") {
		t.Errorf("errMsg = %q, want mention of mutual exclusivity", errMsg)
	}
}

func TestResolveVaultsWeighted_DefaultEqualWeights(t *testing.T) {
	vaults, weights, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vaults": []any{"a", "b", "c"},
	})
	if !present || errMsg != "" {
		t.Fatalf("unexpected: present=%v err=%q", present, errMsg)
	}
	if len(vaults) != 3 || len(weights) != 3 {
		t.Fatalf("vaults=%v weights=%v, want 3 entries each", vaults, weights)
	}
	for _, w := range weights {
		if w != 1.0/3.0 {
			t.Errorf("weight = %v, want 1/3 for equal default weighting", w)
		}
	}
}

// TestResolveVaultsWeighted_StringifiedArrayAccepted guards the fix for MCP
// clients that stringify array-valued arguments not declared in a tool's
// advertised inputSchema: 'vaults' arriving as "[\"a\",\"b\"]" must be parsed
// as a native array rather than rejected as non-array.
func TestResolveVaultsWeighted_StringifiedArrayAccepted(t *testing.T) {
	vaults, weights, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vaults": `["a","b"]`,
	})
	if !present || errMsg != "" {
		t.Fatalf("stringified vaults array rejected: present=%v err=%q", present, errMsg)
	}
	if len(vaults) != 2 || vaults[0] != "a" || vaults[1] != "b" {
		t.Fatalf("vaults = %v, want [a b]", vaults)
	}
	if len(weights) != 2 {
		t.Fatalf("weights = %v, want 2 equal defaults", weights)
	}
}

func TestResolveVaultsWeighted_StringifiedWeightsAccepted(t *testing.T) {
	vaults, weights, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vaults":  `["a","b"]`,
		"weights": `[0.7, 0.3]`,
	})
	if !present || errMsg != "" {
		t.Fatalf("stringified weights rejected: present=%v err=%q", present, errMsg)
	}
	if len(vaults) != 2 || len(weights) != 2 || weights[0] != 0.7 || weights[1] != 0.3 {
		t.Fatalf("vaults=%v weights=%v, want weights [0.7 0.3]", vaults, weights)
	}
}

func TestResolveVaultsWeighted_ExplicitWeightsMustMatchLength(t *testing.T) {
	_, _, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vaults":  []any{"a", "b"},
		"weights": []any{0.5},
	})
	if !present || !strings.Contains(errMsg, "'weights'") {
		t.Errorf("expected length-mismatch error, got present=%v err=%q", present, errMsg)
	}
}

func TestResolveVaultsWeighted_InvalidVaultNameRejected(t *testing.T) {
	_, _, present, errMsg := resolveVaultsWeighted(nil, map[string]any{
		"vaults": []any{"Bad Name!"},
	})
	if !present || !strings.Contains(errMsg, "invalid vault name") {
		t.Errorf("expected invalid-name error, got present=%v err=%q", present, errMsg)
	}
}

func TestResolveVaultsWeighted_OutOfScopeVaultRejectedNoLeak(t *testing.T) {
	scope := []string{"agent-memory", "proj-*"}
	_, _, present, errMsg := resolveVaultsWeighted(scope, map[string]any{
		"vaults": []any{"agent-memory", "someone-elses-vault"},
	})
	if !present {
		t.Fatal("present should be true")
	}
	if !strings.Contains(errMsg, "vault mismatch") {
		t.Errorf("errMsg = %q, want 'vault mismatch'", errMsg)
	}
	if strings.Contains(errMsg, "agent-memory") || strings.Contains(errMsg, "proj-*") {
		t.Errorf("errMsg leaked scope contents: %q", errMsg)
	}
}

func TestResolveVaultsWeighted_InScopeVaultsAccepted(t *testing.T) {
	scope := []string{"agent-memory", "proj-*"}
	vaults, weights, present, errMsg := resolveVaultsWeighted(scope, map[string]any{
		"vaults": []any{"agent-memory", "proj-muninndb-3f2a1b9c"},
	})
	if !present || errMsg != "" {
		t.Fatalf("unexpected: present=%v err=%q", present, errMsg)
	}
	if len(vaults) != 2 || len(weights) != 2 {
		t.Fatalf("vaults=%v weights=%v", vaults, weights)
	}
}

// ── recordingEngine: fakeEngine that records vaults touched, for handler-level assertions ──

type recordingEngine struct {
	fakeEngine
	writes          []*mbp.WriteRequest
	activateVaults  []string
	activateWeights []float64
}

func (e *recordingEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.writes = append(e.writes, req)
	return &mbp.WriteResponse{ID: "id-" + req.Vault}, nil
}

func (e *recordingEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	e.writes = append(e.writes, reqs...)
	resps := make([]*mbp.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i, r := range reqs {
		resps[i] = &mbp.WriteResponse{ID: "id-" + r.Vault}
	}
	return resps, errs
}

func (e *recordingEngine) ActivateMulti(ctx context.Context, reqs []*mbp.ActivateRequest, weights []float64) (*mbp.ActivateResponse, error) {
	for _, r := range reqs {
		e.activateVaults = append(e.activateVaults, r.Vault)
	}
	e.activateWeights = weights
	items := make([]mbp.ActivationItem, len(reqs))
	for i, r := range reqs {
		items[i] = mbp.ActivationItem{ID: "item-" + r.Vault, Vault: r.Vault, Concept: "c"}
	}
	return &mbp.ActivateResponse{Activations: items, TotalFound: len(items), DegradedVaults: []string{"stale-vault"}}, nil
}

// ── handleRemember / handleRememberBatch multi-vault fan-out ───────────────

func TestHandleRemember_MultiVault_WritesOnePerVault(t *testing.T) {
	eng := &recordingEngine{}
	srv := newTestServerWith(eng)
	body := mkToolCallBody("muninn_remember", map[string]any{
		"vaults":  []any{"proj-a", "agent-memory"},
		"concept": "x",
		"content": "y",
	})
	w := doAuthenticatedPost(srv, "", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(eng.writes) != 2 {
		t.Fatalf("engine.Write called %d times, want 2 (one per vault)", len(eng.writes))
	}
	gotVaults := map[string]bool{}
	for _, w := range eng.writes {
		gotVaults[w.Vault] = true
	}
	if !gotVaults["proj-a"] || !gotVaults["agent-memory"] {
		t.Errorf("writes went to vaults %v, want proj-a and agent-memory", gotVaults)
	}
}

func TestHandleRemember_VaultAndVaultsMutuallyExclusive(t *testing.T) {
	eng := &recordingEngine{}
	srv := newTestServerWith(eng)
	body := mkToolCallBody("muninn_remember", map[string]any{
		"vault":   "a",
		"vaults":  []any{"a", "b"},
		"concept": "x",
		"content": "y",
	})
	w := doAuthenticatedPost(srv, "", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error for vault+vaults mutual exclusivity")
	}
}

func TestHandleRememberBatch_MultiVault_WritesOnePerVaultPerMemory(t *testing.T) {
	eng := &recordingEngine{}
	srv := newTestServerWith(eng)
	body := mkToolCallBody("muninn_remember_batch", map[string]any{
		"vaults": []any{"proj-a", "agent-memory"},
		"memories": []any{
			map[string]any{"concept": "x1", "content": "y1"},
			map[string]any{"concept": "x2", "content": "y2"},
		},
	})
	w := doAuthenticatedPost(srv, "", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	// 2 memories × 2 vaults = 4 independent engrams.
	if len(eng.writes) != 4 {
		t.Fatalf("engine.WriteBatch wrote %d engrams, want 4 (2 memories x 2 vaults)", len(eng.writes))
	}
}

// ── handleRecall multi-vault merged recall ──────────────────────────────────

func TestHandleRecall_MultiVault_CallsActivateMultiAndSurfacesDegraded(t *testing.T) {
	eng := &recordingEngine{}
	srv := newTestServerWith(eng)
	body := mkToolCallBody("muninn_recall", map[string]any{
		"vaults":  []any{"proj-a", "agent-memory"},
		"weights": []any{0.67, 0.33},
		"context": "test",
	})
	w := doAuthenticatedPost(srv, "", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(eng.activateVaults) != 2 {
		t.Fatalf("ActivateMulti called with %d vault requests, want 2", len(eng.activateVaults))
	}
	if eng.activateWeights[0] != 0.67 || eng.activateWeights[1] != 0.33 {
		t.Errorf("weights = %v, want [0.67 0.33]", eng.activateWeights)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["degraded_vaults"]; !ok {
		t.Error("response missing degraded_vaults field when ActivateMulti reported one")
	}
}

func TestHandleRecall_VaultAndVaultsMutuallyExclusive(t *testing.T) {
	eng := &recordingEngine{}
	srv := newTestServerWith(eng)
	body := mkToolCallBody("muninn_recall", map[string]any{
		"vault":   "a",
		"vaults":  []any{"a", "b"},
		"context": "test",
	})
	w := doAuthenticatedPost(srv, "", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error for vault+vaults mutual exclusivity")
	}
}

func TestHandleRecall_MultiVault_ScopeViolationFailsWholeCallNoLeak(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:     "scoped001",
		Vaults: []string{"agent-memory", "proj-*"},
		Mode:   auth.ModeFull,
	})
	eng := &recordingEngine{}
	srv := New(":0", eng, "mdb_static", store, nil, nil)
	body := mkToolCallBody("muninn_recall", map[string]any{
		"vaults":  []any{"agent-memory", "someone-elses-vault"},
		"context": "test",
	})
	w := doAuthenticatedPost(srv, "mk_scoped001", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected scope-violation error")
	}
	if !strings.Contains(resp.Error.Message, "vault mismatch") {
		t.Errorf("error = %q, want 'vault mismatch'", resp.Error.Message)
	}
	if strings.Contains(resp.Error.Message, "agent-memory") || strings.Contains(resp.Error.Message, "proj-*") {
		t.Errorf("error leaked scope contents: %q", resp.Error.Message)
	}
	if len(eng.activateVaults) != 0 {
		t.Errorf("ActivateMulti must not be called when scope check fails, got calls for %v", eng.activateVaults)
	}
}

func TestHandleRecall_MultiVault_AllInScopeSucceeds(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:     "scoped002",
		Vaults: []string{"agent-memory", "proj-*"},
		Mode:   auth.ModeFull,
	})
	eng := &recordingEngine{}
	srv := New(":0", eng, "mdb_static", store, nil, nil)
	body := mkToolCallBody("muninn_recall", map[string]any{
		"vaults":  []any{"agent-memory", "proj-muninndb-3f2a1b9c"},
		"context": "test",
	})
	w := doAuthenticatedPost(srv, "mk_scoped002", body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(eng.activateVaults) != 2 {
		t.Fatalf("ActivateMulti called with %d vault requests, want 2", len(eng.activateVaults))
	}
}
