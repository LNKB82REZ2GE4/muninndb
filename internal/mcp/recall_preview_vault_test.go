package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const previewVaultLongBody = "pelican harbour schedule "

// longRowEngine returns one long row per recall: from the LAST requested vault
// on a multi-vault recall (so not the caller's default), or from the single
// vault on a plain recall.
type longRowEngine struct{ recordingEngine }

func (e *longRowEngine) ActivateMulti(ctx context.Context, reqs []*mbp.ActivateRequest, weights []float64) (*mbp.ActivateResponse, error) {
	v := reqs[len(reqs)-1].Vault
	items := []mbp.ActivationItem{{ID: "01HXAAAAAAAAAAAAAAAAAAAAAA", Vault: v, Concept: "long page", Content: strings.Repeat(previewVaultLongBody, 300)}}
	return &mbp.ActivateResponse{Activations: items, TotalFound: 1}, nil
}

func (e *longRowEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	items := []mbp.ActivationItem{{ID: "01HXAAAAAAAAAAAAAAAAAAAAAA", Vault: req.Vault, Concept: "long page", Content: strings.Repeat(previewVaultLongBody, 300)}}
	return &mbp.ActivateResponse{Activations: items, TotalFound: 1}, nil
}

func recallRows(t *testing.T, args map[string]any) []map[string]any {
	t.Helper()
	srv := newTestServerWith(&longRowEngine{})
	w := doAuthenticatedPost(srv, "", mkToolCallBody("muninn_recall", args))
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	content := extractInnerJSON(t, resp)
	raw, _ := json.Marshal(content["memories"])
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	return rows
}

// F1: on a multi-vault recall a truncated row's marker must be followable.
// muninn_read resolves an id in one vault (the caller's default unless told),
// so the row has to say which vault it came from — in the marker and as a
// field.
func TestRecallPreview_MultiVaultMarkerNamesTheRowsVault(t *testing.T) {
	row := recallRows(t, map[string]any{"vaults": []any{"proj-a", "notes-b"}, "context": "pelican"})[0]
	c, _ := row["content"].(string)
	if want := "muninn_read 01HXAAAAAAAAAAAAAAAAAAAAAA vault=notes-b]"; !strings.HasSuffix(c, want) {
		t.Errorf("marker %q does not end with %q", c[strings.LastIndex(c, "…"):], want)
	}
	if v, _ := row["vault"].(string); v != "notes-b" {
		t.Errorf("row vault = %q, want notes-b", v)
	}
}

// The single-vault format is unchanged: bare-id marker, no vault field.
func TestRecallPreview_SingleVaultMarkerUnchanged(t *testing.T) {
	row := recallRows(t, map[string]any{"vault": "proj-a", "context": "pelican"})[0]
	c, _ := row["content"].(string)
	if want := "muninn_read 01HXAAAAAAAAAAAAAAAAAAAAAA]"; !strings.HasSuffix(c, want) {
		t.Errorf("marker %q does not end with %q", c[strings.LastIndex(c, "…"):], want)
	}
	if _, ok := row["vault"]; ok {
		t.Errorf("single-vault row carries a vault field: %v", row["vault"])
	}
}
