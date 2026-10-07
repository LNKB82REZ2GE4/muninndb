package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// G2: muninn_read returned summary, entities and relationships for an enriched
// engram but never its key points, although the engine's ReadResponse (and REST)
// carried them: readResponseToMemory dropped the field.
func TestReadResponseToMemory_MapsKeyPoints(t *testing.T) {
	kps := []string{"The cache is flushed nightly.", "Backups are verified weekly."}
	m := readResponseToMemory(&mbp.ReadResponse{ID: "r", Content: "c", KeyPoints: kps})
	if len(m.KeyPoints) != 2 || m.KeyPoints[0] != kps[0] || m.KeyPoints[1] != kps[1] {
		t.Fatalf("KeyPoints = %q, want %q", m.KeyPoints, kps)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"key_points":["The cache is flushed nightly.","Backups are verified weekly."]`) {
		t.Fatalf("muninn_read JSON lacks key_points: %s", b)
	}
}

func TestActivationToMemory_MapsMatchedKeyPoint(t *testing.T) {
	m := activationToMemory(&mbp.ActivationItem{ID: "a", Content: "c", MatchedKeyPoint: "Backups are verified weekly."})
	if m.MatchedKeyPoint != "Backups are verified weekly." {
		t.Fatalf("MatchedKeyPoint = %q", m.MatchedKeyPoint)
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"matched_key_point":"Backups are verified weekly."`) {
		t.Fatalf("muninn_recall row JSON lacks matched_key_point: %s", b)
	}
	// Absent, not empty, when nothing matched.
	b, _ = json.Marshal(activationToMemory(&mbp.ActivationItem{ID: "b", Content: "c"}))
	if strings.Contains(string(b), "matched_key_point") || strings.Contains(string(b), "key_points") {
		t.Fatalf("unmatched row carries key-point fields: %s", b)
	}
}
