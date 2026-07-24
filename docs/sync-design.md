# fuji-cull — Offline-First Cross-Device Progress Sync

Status: **in progress** (built on branch `feat/cross-device-sync`). This document is
the design of record. It was produced by an exhaustive code-map pass and a
red-teamed design pass (28 data-loss scenarios found and folded in as `[FIX-n]`).

## Goal

Start culling on one device, resume on another. Each client keeps a full local
copy of its per-camera decisions and syncs up to a **self-hosted server** when it
can; other clients pull and resume where the first left off. Fully offline-first,
sync is optional and inert unless configured.

## Decisions adopted (made autonomously; each is reversible — flagged for review)

1. **Self-hosted server: yes** — a small Go relay (`cmd/fuji-sync`) + Docker/compose.
   It is only a relay; every device keeps its full local copy, so the server is
   never the sole copy of any decision.
2. **Auth: single shared `x-api-key`** — mirrors the existing Immich client. Per-device
   tokens are a drop-in later (`deviceId` is already threaded through the protocol).
3. **Resume point: synced, opt-in** — a tappable "resume at DSCF0007 (iPad)" chip that
   never yanks your live scroll.
4. **Card identity: body serial + lightweight card fingerprint** for v1 (warns on a
   suspected swap / dual-card overflow); true hardware card-ID capture deferred.
5. **Upgrade is one-way** — migration is non-destructive and invisible; do not run a
   pre-v2 binary against a v2 file afterward.

## Core model

Progress = per-camera `{canonicalKey → keep|reject|tombstone}` + per-device resume
point. Merge is **server-authoritative delta sync** with an identical HLC-LWW
re-merge on the engine side.

### Canonical shot key (the linchpin)

Today `Shot.ID = it.Dir + "/" + base`, and `it.Dir` differs per backend
(`SLOT 1/DCIM/151_FUJI` on desktop/Android, `DCIM/151_FUJI` on the iOS PTP path),
so decisions do **not** line up across platforms. Verified empirically against the
live iPad: its real keys are `SLOT 1/DCIM/151_FUJI/DSCF8558`.

Fix — **dual-key, never an ID rewrite**: add a new field
`Shot.CanonicalKey = Folder + "/" + Base` (e.g. `151_FUJI/DSCF0001`). `Folder` is the
`photo.FolderRe`-matched `NNN_FUJI` bucket and `Base` is the `SplitMedia` stem —
both backend-independent. `Shot.ID` is left untouched (thumb/fetch caches keyed on
it keep resolving; dual-slot twins keep distinct IDs so `cat.Index`/`counts()` don't
corrupt). Two reverse indexes are added beside `cat.Index`:

- `Catalog.Canonical map[legacyID]canonicalKey` (1:1) — the **outbound/write** resolver.
- `Catalog.Legacy map[canonicalKey][]legacyID` (1:N) — the **inbound/projection** resolver.

Dual-slot overflow guard: if two shots share `(Folder,Base)` but differ in size/date
they are distinct exposures and the key gets a `#cardFingerprint` suffix (+ a
`/api/status.sync` warning); identical twins (backup mode) share one register.

### Migration (preserve the live iPad decisions)

`loadSession` reads v1 files unchanged; `Records` are derived from legacy `Decisions`
on load by **parsing the key string** (`canonicalizeLegacyKey`: last two segments iff
the penultimate matches `FolderRe`) — no catalog needed, immune to discovery path.
N:1 collapse (pre/post PTP-fix keys for one frame) is **deterministic** (sorted
iteration, prefer PTP-shaped key, `reject > keep` on true conflict) and **logged**.
Migrated records carry `Migrated:true` and **lose to any genuine post-v2 edit**. The
pre-identity `default.json` is **merged** (newer-file-wins per key), not orphaned.

### Session schema v2 (backward-compatible)

`sessionData` gains `Version, DeviceID, NodeHLC, Camera, ServerVer, Epoch`, plus
`Records map[canonicalKey]record` (authoritative) and `Resume map[deviceId]cursorRec`.
Legacy `Decisions`/`Cursor` (unchanged tags/types) are kept as a **projection** so
every existing reader (`Decisions()`, `/api/status`, iOS/desktop) is byte-identical.
`record{D, Del, HLC, SV, Migrated}`; `SV==0` **is** the outbox (no separate file).
`hlc{Wall, Ctr, Dev, Nonce}`.

### Merge (HLC-LWW + tombstones)

One LWW register per canonicalKey; a **clear is a tombstone** (`Del:true`), never a
key delete (fixes the current resurrection hazard). Hybrid Logical Clock ordered by
`(wall, ctr, dev, nonce)`; `NodeHLC` persisted and seeded to max-on-load; incoming
`wall > server_now+24h` clamped; migrated loses to non-migrated; genuinely-concurrent
skewed edits are surfaced as a non-destructive **contested** chip, never silently
dropped. `Session.ApplyRemote` batch-applies under one lock / one save, commutative +
idempotent.

### Cursor / resume

Never sync the raw int. Captured at the `Session.SetCursor` chokepoint via
`PendingCursor`, resolved to a canonicalKey lazily at push time (where the catalog
exists), stored per-device in `Resume`. Inbound is advisory (a chip), never forces
the live scroll.

## The server (`cmd/fuji-sync`)

Go + `modernc.org/sqlite` (pure Go, distroless static image) — imported only under
`cmd/fuji-sync`, never in `internal/cull` (keeps it out of the gomobile binary).
Monotonic `version` per camera slug (bumped in the same `BEGIN IMMEDIATE` txn as each
accepted row) gives a total order for pull-since. **Generation guard**: persist
`high_version_ever`; if a restored DB is rewound, mint a fresh `epoch`; clients on an
epoch/rewind mismatch reset and full-reseed. Endpoints (all `x-api-key`, health
exempt): `POST /api/sync/push`, `GET /api/sync/pull?camera=&since=`, `GET /api/sync/health`.
Deploy: `deploy/sync/{Dockerfile,compose.yml}`.

## Engine integration

- Hook `Session.SetDecision`/`SetCursor` (catches HTTP handlers + in-process desktop).
- Resolvers injected at `finishInit`; full re-projection sweep before ready so
  pre-discovery pulls don't show UNDECIDED.
- Cross-platform identity: add `mtpcli.DeviceInfo` + `cliBackend.CameraIdentity()`
  (desktop/Android), close the iOS objects-fallback identity gap; refuse to sync under
  a degenerate (serial-less) identity.
- Config: `SyncURL`/`SyncKey` on `cull.Options`, threaded via `mobile.SetEnv`
  (`FUJI_SYNC_URL`/`FUJI_SYNC_KEY`) on mobile + `--sync-url`/`--sync-key` on desktop.
- Outbox = the session file (`SV==0`); syncer loop with escalating backoff (retry
  forever), wake-on-event, launched from the persisted slug independent of discovery.
- Sync status folded into `/api/status.sync` (ignore-if-absent → un-updated clients
  keep working).

## Phased plan (each independently shippable)

1. **Canonical key + robust migration** (local only — ships value immediately; also
   de-dups the pre/post-PTP-fix key split already on the iPad).
2. **Session v2 schema + chokepoint + HLC** (local-only, invisible).
3. **Cross-platform + non-degenerate camera identity.**
4. **Sync server** (standalone, curl/test-verifiable).
5. **Engine sync client** (first end-to-end; integration test proves two engines converge).
6. **Per-client config + inbound-apply + sync-status UI** (iOS/Android/web/desktop).
7. **Cursor/resume + card-generation reset** (opt-in, non-gating).

Full red-teamed design with all `[FIX-n]` rationale: see the design workflow output
referenced in the PR.
