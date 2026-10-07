package rest

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

// embedHealthMockEngine is a MockEngine that reports embed-backend health.
type embedHealthMockEngine struct {
	MockEngine
	snap plugin.EmbedHealthSnapshot
}

func (e *embedHealthMockEngine) EmbedHealth() plugin.EmbedHealthSnapshot { return e.snap }

// TestHealthEndpoint_ReportsEmbedBackend pins G4 on REST: GET /api/health
// carries embed_backend so a poller can alert on a silent BM25 fall-back, and
// it withholds the provider error text (the endpoint is unauthenticated).
func TestHealthEndpoint_ReportsEmbedBackend(t *testing.T) {
	eng := &embedHealthMockEngine{snap: plugin.EmbedHealthSnapshot{
		State: plugin.EmbedHealthDegraded, ConsecutiveFailures: 2,
		LastError: "provider transport failed: dial tcp 127.0.0.1:1: connection refused",
	}}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.EmbedBackend == nil || resp.EmbedBackend.State != plugin.EmbedHealthDegraded || resp.EmbedBackend.ConsecutiveFailures != 2 {
		t.Fatalf("embed_backend = %+v, want degraded with 2 failures", resp.EmbedBackend)
	}
	if resp.EmbedBackend.LastError != "" {
		t.Fatalf("public health leaked the provider error: %q", resp.EmbedBackend.LastError)
	}
}
