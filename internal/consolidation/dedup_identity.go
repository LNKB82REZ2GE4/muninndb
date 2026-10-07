package consolidation

import (
	"strings"
	"unicode"

	"github.com/scrypster/muninndb/internal/storage"
)

// Identity and polarity separation for dedup — the second stage of the
// pattern-separation guard in dedup_separation.go, closing two of the blind
// spots that file documents.
//
// IDENTITY. "Priya owns the release" and "Marcus owns the release" embed almost
// identically and are different facts. The guard refuses a merge when EACH side
// carries a name-like token the other side does not contain at all — a swap.
// One-sided additions ("owned by Priya" vs "owned by Priya, with Marcus as
// backup") are elaborations, not swaps, and stay mergeable: requiring both sides
// keeps the guard from blocking every restatement that adds a detail.
//
// A token is name-like by SHAPE, never by a word list (CLAUDE.md principle 11):
//   - an identifier shape: an uppercase letter after the first rune (QueueStore,
//     API, iOS), or an underscore / internal dot / slash joining letters
//     (muninn_recall, engine.go, internal/engine);
//   - a capitalized word in a non-sentence-initial position;
//   - a capitalized word in sentence-initial position ONLY when the vault's own
//     text says that word is a name: the vault profile (built per dedup pass
//     from every live engram in the vault) has seen it mid-sentence, and capitalized
//     in the majority of those mid-sentence occurrences. Without a profile, a
//     sentence-initial word is never treated as a name ("Use uv" vs "Prefer uv"
//     must stay mergeable). The majority rule is a decision boundary, not a
//     tuned constant: it asks "does this vault usually capitalize this word
//     where capitalization is not forced?".
// Only Content is read: Concept is a short label that two true duplicates often
// word differently, and a label swap is not a fact swap.
// Comparison is case-insensitive and against EVERY token of the other side, so
// a name that the other side merely writes in lowercase is not a difference.
//
// POLARITY. dedup_separation.go catches explicit negation words. It cannot see
// a negation carried by morphology: "supported" vs "unsupported", "enabled" vs
// "disabled", "import" vs "export". The guard refuses a merge when a word on one
// side and a word on the other side (each absent from the opposite side) are
// related by an English negating prefix (un-, non-, dis- on a base of at least
// three letters, and in-/im-/il-/ir-
// on a base of at least four letters, so "input"/"put" cannot fire) or by an
// en-/dis- or in-/im- to ex- prefix substitution on a shared stem of at least four
// letters. These are closed morphological classes of the language, the same
// kind of fixed grammar as the negation-marker set, not vocabulary fitted to a
// sample.
//
// Known blind spots, not claimed here: names that differ only in a lowercase
// token ("the office is in boston" vs "... in nyc"), antonyms with no shared
// morphology (true/false, on/off, increase/decrease), and spelled-out numbers.

// caseProfile records, per lowercased word, how the vault writes it where
// capitalization is not forced by sentence position.
type caseProfile struct {
	midCap   map[string]int // mid-sentence occurrences that were capitalized
	midTotal map[string]int // all mid-sentence occurrences
}

func newCaseProfile() *caseProfile {
	return &caseProfile{midCap: map[string]int{}, midTotal: map[string]int{}}
}

// add folds one engram's text into the profile.
func (p *caseProfile) add(e *storage.Engram) {
	for _, tk := range identityTokens(e.Content) {
		if tk.initial || !isAlphaWord(tk.raw) {
			continue
		}
		lw := strings.ToLower(tk.raw)
		p.midTotal[lw]++
		if startsUpper(tk.raw) {
			p.midCap[lw]++
		}
	}
}

// isName reports whether the vault usually capitalizes lw mid-sentence.
func (p *caseProfile) isName(lw string) bool {
	if p == nil {
		return false
	}
	total := p.midTotal[lw]
	return total > 0 && 2*p.midCap[lw] > total
}

type identityToken struct {
	raw     string // original case, surrounding punctuation trimmed
	initial bool   // first word of the text, of a line, or of a sentence
}

// identityTokens splits on whitespace, trims surrounding punctuation (keeping
// internal . _ / - so identifiers and paths survive), and marks
// sentence-initial positions. Pure-punctuation tokens (bullets, "##", "1.")
// do not consume the initial position.
func identityTokens(s string) []identityToken {
	var out []identityToken
	for _, line := range strings.Split(s, "\n") {
		initial := true
		for _, f := range strings.Fields(line) {
			raw := strings.TrimFunc(f, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
			})
			if raw == "" || !hasLetter(raw) {
				// Bullets, headings, list numbers: position unchanged.
				continue
			}
			out = append(out, identityToken{raw: raw, initial: initial})
			last := f[len(f)-1]
			initial = last == '.' || last == '!' || last == '?' || last == ':' || last == ';'
		}
	}
	return out
}

// nameTokens returns the lowercased name-like tokens of e, plus the lowercased
// set of every word it contains (for the "absent from the other side" check).
func nameTokens(e *storage.Engram, prof *caseProfile) (names, all map[string]bool) {
	names, all = map[string]bool{}, map[string]bool{}
	for _, tk := range identityTokens(e.Content) {
		lw := strings.ToLower(tk.raw)
		all[lw] = true
		// Sub-words of an identifier also count as present ("deploy_pipeline"
		// on one side, "deploy pipeline" on the other is not a swap).
		for _, part := range strings.FieldsFunc(lw, func(r rune) bool { return r == '_' || r == '.' || r == '/' || r == '-' }) {
			all[part] = true
		}
		switch {
		case len([]rune(tk.raw)) < 2:
			// Single letters are never names: the pronoun "I" is capitalized
			// everywhere, and list/variable labels ("option A", "N") are not
			// identities this guard can reason about.
		case isIdentifierShape(tk.raw):
			names[lw] = true
		case !startsUpper(tk.raw) || !isAlphaWord(tk.raw):
			// lowercase word, or a capitalized token mixing in digits (the
			// number guard owns those).
		case !tk.initial:
			names[lw] = true
		case prof.isName(lw):
			names[lw] = true
		}
	}
	return names, all
}

// divergesOnIdentity reports a name swap: each side has a name-like token that
// does not occur anywhere in the other side.
func divergesOnIdentity(a, b *storage.Engram, prof *caseProfile) bool {
	na, alla := nameTokens(a, prof)
	nb, allb := nameTokens(b, prof)
	return hasMissing(na, allb) && hasMissing(nb, alla)
}

func hasMissing(names, other map[string]bool) bool {
	for n := range names {
		if !other[n] {
			return true
		}
	}
	return false
}

// divergesOnPolarity reports a morphological negation between the two sides.
func divergesOnPolarity(a, b *storage.Engram) bool {
	wa := wordSet(loadBearingText(a))
	wb := wordSet(loadBearingText(b))
	var onlyA, onlyB []string
	for w := range wa {
		if !wb[w] {
			onlyA = append(onlyA, w)
		}
	}
	for w := range wb {
		if !wa[w] {
			onlyB = append(onlyB, w)
		}
	}
	for _, x := range onlyA {
		for _, y := range onlyB {
			if polarityPair(x, y) || polarityPair(y, x) {
				return true
			}
		}
	}
	return false
}

// polarityPair reports whether neg is a morphological negation of pos.
func polarityPair(pos, neg string) bool {
	for _, p := range []string{"un", "non", "dis"} {
		if len(pos) >= 3 && neg == p+pos {
			return true
		}
	}
	for _, p := range []string{"in", "im", "il", "ir"} {
		if len(pos) >= 4 && neg == p+pos {
			return true
		}
	}
	// Prefix substitutions on a shared stem: enable/disable, include/exclude,
	// import/export, internal/external.
	for _, sub := range [][2]string{{"en", "dis"}, {"in", "ex"}, {"im", "ex"}} {
		if strings.HasPrefix(pos, sub[0]) && strings.HasPrefix(neg, sub[1]) {
			stem := pos[len(sub[0]):]
			if len(stem) >= 4 && stem == neg[len(sub[1]):] {
				return true
			}
		}
	}
	return false
}

// wordSet tokenizes lowercased text into words of letters and hyphens, with
// the hyphens removed, so "non-blocking" is seen as "nonblocking" and pairs
// with "blocking" the same way the unhyphenated spelling does.
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' }) {
		if w = strings.ReplaceAll(w, "-", ""); w != "" {
			out[w] = true
		}
	}
	return out
}

func isIdentifierShape(s string) bool {
	if !hasLetter(s) {
		return false
	}
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
		if r == '_' || ((r == '.' || r == '/') && i > 0 && i < len(runes)-1) {
			return true
		}
	}
	return false
}

func startsUpper(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

func isAlphaWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && r != '\'' && r != '-' {
			return false
		}
	}
	return s != ""
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
