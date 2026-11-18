// Package prefix is the single source of truth for Pebble key-prefix byte
// allocations across storage, auth, capability, and replication. Every key
// constructor in internal/storage/keys, internal/auth and internal/replication
// MUST reference these constants; never inline a raw byte. [RT-FIX RT3] The
// length invariants below are load-bearing for the v3 migration discriminator.
//
// #726: replication used to bypass this registry entirely and live under 0x19,
// byte-for-byte overlapping storage's 0x19 Idempotency (both were
// 0x19|8-bytes). It now owns Replication (0x2F) and references it from here;
// migration v5 relocates existing vaults.
package prefix

// Source-of-truth prefix bytes. Storage unchanged; auth RELOCATED 0x11–0x14 → 0x42–0x45.
const (
	// Storage (0x01–0x2E, 0x30, 0x31; 0x2F is replication's; fork marker at 0xF0)
	Engram             byte = 0x01
	Meta               byte = 0x02
	AssocFwd           byte = 0x03
	AssocRev           byte = 0x04
	FTSPosting         byte = 0x05
	Trigram            byte = 0x06
	HNSWNode           byte = 0x07
	FTSStats           byte = 0x08
	TermStats          byte = 0x09
	Contradiction      byte = 0x0A
	StateIndex         byte = 0x0B
	TagIndex           byte = 0x0C
	CreatorIndex       byte = 0x0D
	VaultMeta          byte = 0x0E
	VaultNameIndex     byte = 0x0F
	RelevanceBucket    byte = 0x10
	DigestFlags        byte = 0x11
	Coherence          byte = 0x12
	VaultWeights       byte = 0x13
	AssocWeightIndex   byte = 0x14
	VaultCount         byte = 0x15
	Provenance         byte = 0x16
	BucketMigration    byte = 0x17
	Embedding          byte = 0x18
	Idempotency        byte = 0x19
	Episode            byte = 0x1A
	FTSVersion         byte = 0x1B
	Transition         byte = 0x1C
	EmbedModel         byte = 0x1D
	Ordinal            byte = 0x1E
	Entity             byte = 0x1F
	EntityEngramLink   byte = 0x20
	Relationship       byte = 0x21
	LastAccess         byte = 0x22
	EntityReverseIndex byte = 0x23
	CoOccurrence       byte = 0x24
	ArchiveAssoc       byte = 0x25
	RelEntityIndex     byte = 0x26
	DreamState         byte = 0x27
	ContentHash        byte = 0x28
	RecallEvent        byte = 0x29
	Lease              byte = 0x2A
	// EvolveRepairMark (0x2B) — vault-scoped watermark for the one-time startup
	// repair of evolve-stripped successors (#681/#622). Idempotent: presence of
	// the mark means the repair already ran for that vault.
	EvolveRepairMark byte = 0x2B
	// RawTagRange (0x2C) — ordered raw-tag secondary index (S1). Unlike
	// TagIndex (0x0C, keyed by Hash(tag) with no range scans), RawTagRange keys
	// on Hash(tagKey) with the raw tag VALUE bytes sorted after it, enabling
	// bounded range scans for key:value tag conventions (e.g. "due:2026-07-27").
	// See docs/internals/keyspace-registry.md for the exact key layout.
	RawTagRange byte = 0x2C
	// ProspectiveIntent (0x2D) — armed-intention index for prospective memory
	// (THE PUSH increment 1). One key per cue entity:
	// 0x2D | ws(8) | EntityNameHash(cue)(8) | intentionID(16) = 33 bytes.
	// Value: msgpack {one_shot, created_at, fired_count, last_fired_at, cues}.
	// NOTE: the design doc allocated 0x2C, but 0x2C was taken by RawTagRange
	// (S1) before this landed — 0x2D is the actual allocation.
	ProspectiveIntent byte = 0x2D
	// AssocWeightRepairMark (0x2E) — vault-scoped watermark for the one-time
	// startup repair of pre-fix full-weight association keys (#756; encoder
	// fixed in #757). The original WeightComplement overflowed at weight
	// exactly 1.0 and wrote those edges at the weight-0.0 key position; the
	// repair relocates them to the true 1.0 position. Presence of the mark at
	// the current version means the repair already ran for that vault. A
	// one-shot watermark is sound because the fixed encoder cannot create new
	// damage of this kind.
	AssocWeightRepairMark byte = 0x2E
	// Replication (0x2F) — the whole internal/replication keyspace, relocated
	// off the double-allocated 0x19 by #726. A second discriminator byte
	// partitions it so that the sequence-keyed log entries can never share an
	// address with anything else:
	//
	//	0x2F | 0x01 | seq_be64(8)  = 10 bytes  log entry (msgpack)
	//	0x2F | 0x02 | name...                  replication metadata
	//
	// The entry sub-range is exactly [0x2F 0x01, 0x2F 0x02), so ReplicationLog.
	// Prune's DeleteRange is STRUCTURALLY confined to log entries — it cannot
	// reach the metadata keys, and (the #726 defect) it cannot reach an
	// idempotency receipt, which now lives a whole prefix away.
	// See internal/replication/keys.go for the constructors.
	Replication byte = 0x2F
	// UpsertKey (0x30) — durable upsert forward index (#556): sha256 of the
	// caller's idempotent_id → the engram ID it is pinned to. Relocation
	// history: 0x2B → 0x2D → 0x2E → 0x2F → 0x30 (0x2F taken by Replication,
	// #726).
	UpsertKey byte = 0x30
	// SupersedesRestoreMark (0x31) — vault-scoped watermark for the one-time
	// restore of the supersedes edges an older binary's association decay
	// archived to 0x25 (2026-11-16). It is its own key, not a value of 0x2E,
	// so the two passes keep separate version sequences: a later bump of
	// either one's version cannot skip or repeat the other.
	SupersedesRestoreMark byte = 0x31
	// Capability (0x40/0x41 — clean since #612)
	Capability         byte = 0x40
	CapabilityVaultIdx byte = 0x41
	// Auth (RELOCATED 0x42–0x45 by #611)
	AdminUser      byte = 0x42
	APIKey         byte = 0x43
	APIKeyVaultIdx byte = 0x44
	VaultConfig    byte = 0x45
	// APIKeyGlobIdx (0x46) — glob-scope API key index (project-vaults fork).
	// Previously inlined as 0x29, which collided with storage's RecallEvent
	// prefix; relocated here during the v0.10.0 merge.
	APIKeyGlobIdx byte = 0x46
	// DeclaredContradictionMark (0xF0) — per-vault O(1) answer to "does this
	// vault contain ANY declared `contradicts` association?", the fast-path
	// gate for COG-29 contradiction honesty (fork-only). Key: 0xF0 | ws(8) =
	// 9 bytes. Value: 1 byte — 0x01 declared (sticky), 0x00 proven clean.
	// ABSENT means UNKNOWN and must never be read as clean; see
	// keys.DeclaredContradictionMarkKey and docs/internals/keyspace-registry.md.
	//
	// RELOCATION HISTORY (fork): 0x2F → 0x31 → 0xF0.
	//
	//   - 0x2F (original allocation). Upstream #726 claimed 0x2F for the
	//     relocated Replication keyspace and #556 claimed 0x30 for UpsertKey,
	//     so both are RESERVED FOR UPSTREAM. The collision was not merely a
	//     registry clash: upstream's replication layout is 0x2F|0x01|seq_be64(8)
	//     for log entries and 0x2F|0x02|name for metadata, while this marker's
	//     key was 0x2F|ws(8) — a vault hash whose first byte happened to be
	//     0x01 or 0x02 would have landed inside upstream's log-entry or metadata
	//     sub-range. Migration v7 (PurgeLegacyDeclaredContradictionMark) removes
	//     every stale 0x2F marker key; it is ordered BEFORE the renumbered
	//     upstream replication relocation (v8).
	//
	//   - 0x31 (pre-0.11.0 merge to the 0.12.1 merge). Upstream #952 then
	//     allocated 0x31 to SupersedesRestoreMark with a BYTE-IDENTICAL record
	//     shape (key 0x31|ws(8), value one byte, version 1 == our "declared"),
	//     so a stale marker reading 0x01 would make upstream's one-time
	//     supersedes restore believe it had already run. Migration v11
	//     (PurgeDeclaredContradictionMarkAt0x31) purges our records off 0x31
	//     before any binary that knows SupersedesRestoreMark can write there,
	//     and rebuilds the marker here.
	//
	// WHY 0xF0. Twice now the fork took the next free byte in upstream's own
	// growth path and upstream walked into it. Upstream allocates storage
	// bytes sequentially upward (0x01 → 0x31; its registry names 0x32–0x3F as
	// the next storage range, "prefer 0x32+") and auth/capability upward from
	// 0x40 (0x40–0x45, then 0x46+, where the fork already sits at 0x46). 0xFF
	// holds upstream's ASCII-named global singletons (mig_ver, vcount_clean).
	// 0xF0 is far outside every range upstream has used or advertised, below
	// the 0xFF singleton byte, and unreferenced anywhere in the tree, so it is
	// the fork's own corner: future fork-only prefixes should go 0xF1, 0xF2…
	DeclaredContradictionMark byte = 0xF0

	// [RT-FIX RT3] length invariants the v3 discriminator depends on (no magic numbers).
	MinAPIKeyLen     = 17 // APIKey key = 1 + 16-byte hash
	MinAPIKeyVIdxLen = 10 // APIKeyVaultIdx key = 1 + vault + 0x00 + 8 (>= 10)
	CoherenceKeyLen  = 9  // Coherence/VaultWeights key = 1 + 8-byte wsPrefix
)

type Entry struct {
	Byte  byte
	Owner string // "storage" | "auth" | "capability" | "replication"
	Name  string
	Cat   string // category for the per-list partition guards (Task 5)
}

func All() []Entry { return registry }

// Category returns the partition category for a prefix (Task 5's per-list guards).
func Category(b byte) string {
	for _, e := range registry {
		if e.Byte == b {
			return e.Cat
		}
	}
	return ""
}

var registry = []Entry{
	{Engram, "storage", "Engram", "vault-scoped-data"},
	{Meta, "storage", "Meta", "vault-scoped-data"},
	{AssocFwd, "storage", "AssocFwd", "vault-scoped-data"},
	{AssocRev, "storage", "AssocRev", "vault-scoped-data"},
	{FTSPosting, "storage", "FTSPosting", "vault-scoped-data"},
	{Trigram, "storage", "Trigram", "vault-scoped-data"},
	{HNSWNode, "storage", "HNSWNode", "vault-scoped-data"},
	{FTSStats, "storage", "FTSStats", "vault-scoped-data"},
	{TermStats, "storage", "TermStats", "vault-scoped-data"},
	{Contradiction, "storage", "Contradiction", "vault-scoped-data"},
	{StateIndex, "storage", "StateIndex", "vault-scoped-data"},
	{TagIndex, "storage", "TagIndex", "vault-scoped-data"},
	{CreatorIndex, "storage", "CreatorIndex", "vault-scoped-data"},
	{VaultMeta, "storage", "VaultMeta", "name-key"},
	{VaultNameIndex, "storage", "VaultNameIndex", "name-key"},
	{RelevanceBucket, "storage", "RelevanceBucket", "vault-scoped-data"},
	{DigestFlags, "storage", "DigestFlags", "global-by-ulid"},
	{Coherence, "storage", "Coherence", "vault-scoped-data"},
	{VaultWeights, "storage", "VaultWeights", "vault-scoped-data"},
	{AssocWeightIndex, "storage", "AssocWeightIndex", "vault-scoped-data"},
	{VaultCount, "storage", "VaultCount", "vault-scoped-data"},
	{Provenance, "storage", "Provenance", "vault-scoped-data"},
	{BucketMigration, "storage", "BucketMigration", "vault-scoped-data"},
	{Embedding, "storage", "Embedding", "vault-scoped-data"},
	{Idempotency, "storage", "Idempotency", "global-by-ulid"},
	{Episode, "storage", "Episode", "vault-scoped-data"},
	{FTSVersion, "storage", "FTSVersion", "vault-scoped-data"},
	{Transition, "storage", "Transition", "vault-scoped-data"},
	{EmbedModel, "storage", "EmbedModel", "vault-scoped-data"},
	{Ordinal, "storage", "Ordinal", "vault-scoped-data"},
	{Entity, "storage", "Entity", "global-by-ulid"},
	{EntityEngramLink, "storage", "EntityEngramLink", "vault-scoped-data"},
	{Relationship, "storage", "Relationship", "vault-scoped-data"},
	{LastAccess, "storage", "LastAccess", "vault-scoped-data"},
	{EntityReverseIndex, "storage", "EntityReverseIndex", "global-by-ulid"},
	{CoOccurrence, "storage", "CoOccurrence", "vault-scoped-data"},
	{ArchiveAssoc, "storage", "ArchiveAssoc", "vault-scoped-data"},
	{RelEntityIndex, "storage", "RelEntityIndex", "vault-scoped-data"},
	{DreamState, "storage", "DreamState", "vault-scoped-data"},
	{ContentHash, "storage", "ContentHash", "vault-scoped-data"},
	{RecallEvent, "storage", "RecallEvent", "vault-scoped-data"},
	{Lease, "storage", "Lease", "lease"},
	{EvolveRepairMark, "storage", "EvolveRepairMark", "vault-scoped-data"},
	{RawTagRange, "storage", "RawTagRange", "vault-scoped-data"},
	{ProspectiveIntent, "storage", "ProspectiveIntent", "vault-scoped-data"},
	{AssocWeightRepairMark, "storage", "AssocWeightRepairMark", "vault-scoped-data"},
	{Replication, "replication", "Replication", "replication"},
	{UpsertKey, "storage", "UpsertKey", "vault-scoped-data"},
	{SupersedesRestoreMark, "storage", "SupersedesRestoreMark", "vault-scoped-data"},
	{Capability, "capability", "Capability", "capability"},
	{CapabilityVaultIdx, "capability", "CapabilityVaultIdx", "capability"},
	{AdminUser, "auth", "AdminUser", "auth"},
	{APIKey, "auth", "APIKey", "auth"},
	{APIKeyVaultIdx, "auth", "APIKeyVaultIdx", "auth"},
	{VaultConfig, "auth", "VaultConfig", "auth"},
	{APIKeyGlobIdx, "auth", "APIKeyGlobIdx", "auth"},
	{DeclaredContradictionMark, "storage", "DeclaredContradictionMark", "vault-scoped-data"},
}
