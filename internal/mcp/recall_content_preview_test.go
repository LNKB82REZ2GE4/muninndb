package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// G11 — muninn_recall content preview.
//
// Before this change every recall row's content was cut at byte 500 and given
// a bare "..." suffix. Three defects in one line:
//
//  1. An agent could not tell HOW MUCH it was not shown, or that the rest was
//     one muninn_read away — a 6 KB knowledge page looked like a 500-byte note
//     that happened to end in an ellipsis (principle #2: never silently wrong).
//  2. The cut was a BYTE offset, so multi-byte UTF-8 content was split mid-rune
//     and serialized as U+FFFD, and content that fit in 500 characters was
//     still cut when it exceeded 500 bytes.
//  3. There was no way to ask recall for the full text, so every long result
//     cost one extra muninn_read round trip.
//
// The fix: a character-bounded, word-boundary preview ending in a visible
// marker that names the hidden length and the read that recovers it,
// structured content_truncated/content_chars fields beside it, and an opt-in
// full_content=true recall argument. The default response stays bounded.

// markerRE matches the truncation marker and captures the hidden-char count
// and the id the marker tells the agent to read.
var markerRE = regexp.MustCompile(` …\[\+([0-9,]+) chars, muninn_read ([^\]]+)\]$`)

// longPage builds a realistic multi-KB "knowledge page": words and line
// breaks, so a word/line boundary exists near any cut point.
func longPage(n int) string {
	var b strings.Builder
	words := []string{"the", "impact", "crater", "depth", "scales", "with", "velocity", "squared", "and", "target", "density"}
	for i := 0; b.Len() < n; i++ {
		b.WriteString(words[i%len(words)])
		if i%17 == 16 {
			b.WriteString("\n")
		} else {
			b.WriteString(" ")
		}
	}
	return b.String()[:n]
}

func parseMarker(t *testing.T, content string) (hidden int, id string, preview string) {
	t.Helper()
	m := markerRE.FindStringSubmatchIndex(content)
	if m == nil {
		t.Fatalf("truncated content carries no visible truncation marker; tail = %q",
			tail(content, 80))
	}
	n, err := strconv.Atoi(strings.ReplaceAll(content[m[2]:m[3]], ",", ""))
	if err != nil {
		t.Fatalf("marker count not a number: %v", err)
	}
	return n, content[m[4]:m[5]], content[:m[0]]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestActivationToMemory_TruncationMarkerIsAccurate(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	page := longPage(6000)
	m := activationToMemory(&mbp.ActivationItem{ID: id, Concept: "page", Content: page})

	hidden, gotID, preview := parseMarker(t, m.Content)
	if gotID != id {
		t.Errorf("marker names id %q, want %q — the agent must be told WHICH memory to read", gotID, id)
	}
	if !strings.HasPrefix(page, preview) {
		t.Fatalf("preview is not a prefix of the real content")
	}
	pr := utf8.RuneCountInString(preview)
	if pr > contentPreviewLen {
		t.Errorf("preview is %d chars, want <= %d — default response size must not grow", pr, contentPreviewLen)
	}
	if pr < contentPreviewLen*3/4 {
		t.Errorf("preview is only %d chars — the word-boundary back-off ate too much", pr)
	}
	// The count is exact: shown + hidden == total.
	if total := utf8.RuneCountInString(page); pr+hidden != total {
		t.Errorf("marker says +%d hidden, preview shows %d: sum %d != real length %d",
			hidden, pr, pr+hidden, total)
	}
	// Word/line boundary: the character right after the preview is whitespace,
	// so the preview never ends on half a word.
	if next := page[len(preview)]; next != ' ' && next != '\n' {
		t.Errorf("preview cut mid-word: next char is %q (preview tail %q)", next, tail(preview, 20))
	}
	// Structured twins of the marker for programmatic callers.
	if !m.ContentTruncated {
		t.Error("content_truncated must be true on a truncated row")
	}
	if m.ContentChars != 6000 {
		t.Errorf("content_chars = %d, want 6000 (the full length)", m.ContentChars)
	}
	// Thousands separator, as an agent reads it: 6000 - ~495 shown.
	if !strings.Contains(m.Content, fmt.Sprintf("+%s chars", commaInt(hidden))) || !strings.Contains(commaInt(hidden), ",") {
		t.Errorf("marker should carry a thousands-separated count, got tail %q", tail(m.Content, 60))
	}
}

func TestCommaInt(t *testing.T) {
	for in, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 5312: "5,312", 1234567: "1,234,567"} {
		if got := commaInt(in); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestActivationToMemory_ShortContentUntouched(t *testing.T) {
	m := activationToMemory(&mbp.ActivationItem{ID: "s", Content: "short note"})
	if m.Content != "short note" || m.ContentTruncated || m.ContentChars != 0 {
		t.Errorf("short content must pass through unmarked: %+v", m)
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "content_truncated") || strings.Contains(string(b), "content_chars") {
		t.Errorf("untruncated row must not carry the truncation fields: %s", b)
	}
	// Exactly at the bound is not truncated either.
	exact := strings.Repeat("a", contentPreviewLen)
	if m := activationToMemory(&mbp.ActivationItem{ID: "e", Content: exact}); m.Content != exact || m.ContentTruncated {
		t.Errorf("content of exactly %d chars must pass through unmarked", contentPreviewLen)
	}
}

// The old cut was a byte offset: 400 two-byte runes (800 bytes, 400 chars) were
// "truncated" although they fit the 500-char preview, and the cut landed
// mid-rune. Now the bound is in characters and the output is valid UTF-8.
func TestActivationToMemory_PreviewIsRuneSafe(t *testing.T) {
	fits := strings.Repeat("é", 400)
	m := activationToMemory(&mbp.ActivationItem{ID: "u1", Content: fits})
	if m.Content != fits || m.ContentTruncated {
		t.Errorf("400-char content was truncated (byte-length bound): len=%d truncated=%v",
			len(m.Content), m.ContentTruncated)
	}

	over := strings.Repeat("日本", 400) // 800 chars, no whitespace at all
	m = activationToMemory(&mbp.ActivationItem{ID: "u2", Content: over})
	if !utf8.ValidString(m.Content) {
		t.Fatal("preview split a multi-byte rune: content is not valid UTF-8")
	}
	hidden, _, preview := parseMarker(t, m.Content)
	if pr := utf8.RuneCountInString(preview); pr != contentPreviewLen || hidden != 800-contentPreviewLen {
		t.Errorf("no-whitespace content: preview %d chars + %d hidden, want %d + %d",
			pr, hidden, contentPreviewLen, 800-contentPreviewLen)
	}
}

// --- handler level: the opt-in and the bounded default ---

type previewFakeEngine struct {
	fakeEngine
	resp *mbp.ActivateResponse
}

func (e *previewFakeEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return e.resp, nil
}

func pagesResponse(n, size int) *mbp.ActivateResponse {
	resp := &mbp.ActivateResponse{TotalFound: n}
	for i := 0; i < n; i++ {
		resp.Activations = append(resp.Activations, mbp.ActivationItem{
			ID:      fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i),
			Concept: fmt.Sprintf("page %d", i),
			Content: longPage(size),
		})
	}
	return resp
}

func recallRaw(t *testing.T, resp *mbp.ActivateResponse, extraArgs string) (map[string]any, int) {
	t.Helper()
	srv := newTestServerWith(&previewFakeEngine{resp: resp})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["anything"]` + extraArgs + `}}}`
	r := decodeResp(t, postRPC(t, srv, body).Body.String())
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	out := extractInnerJSON(t, r)
	b, _ := json.Marshal(out)
	return out, len(b)
}

func TestRecallOverMCP_FullContentOptIn(t *testing.T) {
	page := longPage(6000)
	out, _ := recallRaw(t, pagesResponse(1, 6000), `,"full_content":true`)
	mem := out["memories"].([]any)[0].(map[string]any)
	if mem["content"] != page {
		t.Fatalf("full_content=true returned %d bytes, want the full %d-byte page",
			len(mem["content"].(string)), len(page))
	}
	if _, has := mem["content_truncated"]; has {
		t.Error("full_content=true row must not be flagged truncated")
	}
}

func TestRecallOverMCP_DefaultStaysBounded(t *testing.T) {
	const rows, size = 10, 6000
	out, n := recallRaw(t, pagesResponse(rows, size), "")
	for _, raw := range out["memories"].([]any) {
		mem := raw.(map[string]any)
		c := mem["content"].(string)
		if !markerRE.MatchString(c) {
			t.Errorf("default recall row carries no truncation marker: tail %q", tail(c, 60))
		}
		if mem["content_truncated"] != true {
			t.Error("default recall row over the preview must say content_truncated=true")
		}
	}
	// Bounded: per row at most the preview + marker + fixed metadata; nowhere
	// near the 60 KB the full pages would cost. 1,000 bytes/row covers 500
	// chars of ASCII + the ~60-byte marker + the row's other fields.
	if limit := rows * 1000; n > limit {
		t.Errorf("default recall response is %d bytes for %d rows, want <= %d", n, rows, limit)
	}
	_, full := recallRaw(t, pagesResponse(rows, size), `,"full_content":true`)
	if full < rows*size {
		t.Errorf("full_content response %d bytes < %d of real content", full, rows*size)
	}
}

// Presentation preference: a malformed value falls back to the default
// preview rather than failing the recall (principle #4, fail open).
func TestRecallOverMCP_FullContentMalformedFailsOpen(t *testing.T) {
	out, _ := recallRaw(t, pagesResponse(1, 6000), `,"full_content":"yes"`)
	mem := out["memories"].([]any)[0].(map[string]any)
	if mem["content_truncated"] != true {
		t.Error("a non-bool full_content must fall back to the bounded preview")
	}
}

func TestRecallSchema_DeclaresFullContent(t *testing.T) {
	for _, tool := range allToolDefinitions() {
		if tool.Name != "muninn_recall" {
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		fc, ok := props["full_content"].(map[string]any)
		if !ok {
			t.Fatal("muninn_recall schema does not declare full_content")
		}
		if fc["type"] != "boolean" {
			t.Errorf("full_content type = %v, want boolean", fc["type"])
		}
		return
	}
	t.Fatal("muninn_recall not found in tool definitions")
}
