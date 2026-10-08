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

// Integration of G2 and G11: the preview bound and its visible marker apply to
// Content only. A matched key point longer than the preview bound, on a row
// whose content is itself cut, must come through whole — a truncated snippet
// would defeat the reason it is surfaced on recall.
func TestActivationToMemory_MatchedKeyPointSurvivesPreviewCut(t *testing.T) {
	kp := strings.Repeat("The nightly backup job verifies every archive checksum. ", 20)
	content := strings.Repeat("Unrelated long engram body text that must be cut. ", 40)
	m := activationToMemoryContent(&mbp.ActivationItem{ID: "x", Content: content, MatchedKeyPoint: kp}, false, false)
	if !m.ContentTruncated {
		t.Fatalf("content of %d chars was not cut; fixture no longer exercises the preview", len(content))
	}
	if m.MatchedKeyPoint != kp {
		t.Fatalf("MatchedKeyPoint was altered by the preview: got %d chars, want %d", len(m.MatchedKeyPoint), len(kp))
	}
	if strings.Contains(m.MatchedKeyPoint, "muninn_read") {
		t.Fatalf("preview marker leaked into matched_key_point: %q", m.MatchedKeyPoint)
	}
}
