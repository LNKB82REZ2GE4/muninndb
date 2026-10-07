package mcp

import (
	"encoding/json"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

// embedHealthEngine is a fakeEngine that reports an embed-health snapshot.
type embedHealthEngine struct {
	fakeEngine
	snap plugin.EmbedHealthSnapshot
}

func (e *embedHealthEngine) EmbedHealth() plugin.EmbedHealthSnapshot { return e.snap }

func statusOf(t *testing.T, srv *MCPServer) VaultStatus {
	t.Helper()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"default"}}}`
	resp := decodeResp(t, postRPC(t, srv, body).Body.String())
	if resp.Error != nil {
		t.Fatalf("muninn_status error: %v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Content) == 0 {
		t.Fatalf("decode tool result: %v (%s)", err, raw)
	}
	var st VaultStatus
	if err := json.Unmarshal([]byte(res.Content[0].Text), &st); err != nil {
		t.Fatalf("decode VaultStatus: %v", err)
	}
	return st
}

// TestHandleStatus_ReportsEmbedBackend pins G4 on MCP: muninn_status carries
// the embed backend's health, and a degraded backend turns the vault health
// from its old hard-coded "good" into "degraded".
func TestHandleStatus_ReportsEmbedBackend(t *testing.T) {
	srv := newTestServerWith(&embedHealthEngine{snap: plugin.EmbedHealthSnapshot{
		State: plugin.EmbedHealthDegraded, ConsecutiveFailures: 3, LastError: "401",
	}})
	st := statusOf(t, srv)
	if st.EmbedBackend == nil || st.EmbedBackend.State != plugin.EmbedHealthDegraded {
		t.Fatalf("embed_backend = %+v, want state degraded", st.EmbedBackend)
	}
	if st.Health != "degraded" {
		t.Fatalf("health = %q with a degraded embed backend, want \"degraded\"", st.Health)
	}

	ok := statusOf(t, newTestServerWith(&embedHealthEngine{snap: plugin.EmbedHealthSnapshot{State: plugin.EmbedHealthOK}}))
	if ok.Health != "good" {
		t.Fatalf("health = %q with a healthy embed backend, want \"good\"", ok.Health)
	}
}
