# Game Save Integration Roadmap

This roadmap is the shared delivery contract for `game-save`, OpenSave, and Eden. It is intentionally
ordered around preventing save rollback before adding discovery or presentation features. A feature is
only complete when its automated checks and an isolated test-save scenario pass; it must never be first
validated with a player's only live save.

## Product Boundary

| Component | Responsibility | Must not do |
| --- | --- | --- |
| `game-save` | Store immutable save objects, maintain a per-scope cloud head, enforce compare-and-swap writes, retain conflicts and publish operational state. | Pick a winner from wall-clock timestamps, inspect plaintext Eden saves, or merge files from divergent saves. |
| Eden | Create encrypted, content-addressed snapshots for a title/account scope; prove that local content matches the last confirmed cloud head before advancing it; restore only by an explicit user action. | Promote an unverified, stale, or cyclic pending chain as the cloud head. |
| OpenSave | Discover and back up user-selected save roots, keep each save type in its own namespace, and present local/cloud/conflict state. | Put PC and emulator archives in one namespace, silently overwrite a divergent local archive, or require WebDAV for the native provider. |

`game-save` is the native endpoint for new OpenSave builds. The legacy `/opensave/` WebDAV endpoint remains
only as a compatibility adapter with its own credential and storage directory. It is not the protocol used
by the planned OpenSave synchronization flow.

## Non-Negotiable Safety Rules

1. Content lineage, not file modification time, decides whether a client may advance a cloud head.
2. Every head-changing request is compare-and-swap against a client-confirmed predecessor.
3. A divergent write becomes an immutable conflict copy. Retention must not delete conflict copies.
4. A restore first preserves the replaced non-empty local directory in a unique recovery directory.
5. Each archive belongs to exactly one type namespace: `pc`, `switch`, or `emulator-<id>`.
6. Production saves, especially a user's only save, are never used for a new feature's first sync or
   restore test. Use a separate test title/profile/root or a verified recoverable copy.
7. Android capability is explicit: a discovered path is marked as app-accessible, SAF-accessible, root-only,
   or unavailable. A non-root build never claims it can back up private application data it cannot read.

## Version And Compatibility Matrix

| Component | Baseline | Required behavior before shared-save use | Next release gate |
| --- | --- | --- | --- |
| `game-save` | `v1.4.1` | Eden protocol v2 is required for all Eden writes; OpenSave has immutable typed CAS scopes and both Eden/OpenSave scope reads are read-only. | Run the isolated backup/restore and image-upgrade rows before changing a NAS container. |
| Eden | `v2026.10.25-1` | Uses the v2 protocol, records confirmed content state, preserves restore copies, quarantines unsafe pending chains, creates conflicts instead of overwriting a newer cloud head, and has a read-only per-title status view. | Run the desktop/Android P5 rows against a disposable profile/title. |
| OpenSave desktop | `v2.4.5` | Uses the native provider, with PC/Switch/emulator namespaces isolated and a per-scope status/conflict view. | Run the Windows P5 rows against disposable roots. |
| OpenSave Android | `0.3.15 (20)` | Supports selected Root and SAF roots, type filters, icons/names, batch/automatic backups, and explicit source capability state. | Run the Root/SAF P5 rows; Android artifacts remain CI-only until those rows pass. |

Older Eden clients remain readable but are intentionally unable to write to a v2 `game-save` service. All
devices sharing an Eden save must be upgraded before they are used interchangeably.

## Current Delivery Ledger

This ledger records delivery state, not an automatic deployment instruction. A released server image is
never permission to replace a NAS container, and an Android build is never permission to touch a live save.

| Work item | State | Evidence / next gate |
| --- | --- | --- |
| P1 server lineage, conflict retention, and recovery protection | Delivered | `game-save` protocol-v2 writes and immutable conflict copies are in released server code. |
| P2 server read-only status | Delivered | `game-save v1.4.1` returns Eden scope state without heartbeats; native OpenSave archive, scope, and head reads are also non-mutating. |
| P2 Eden per-title status | Delivered in source | Desktop core build and Android release build pass; P5 validates the artifact on isolated test profiles. |
| P2 OpenSave per-game status | Delivered in source | Typed native status and ordinary-restore conflict blocking are merged; P5 validates client behavior on disposable roots. |
| P3 typed discovery | Delivered in source | `pc`, `switch`, and `emulator-*` remain isolated; Android now reports Root/SAF/unavailable access without treating it as an empty source. P5 validates physical devices. |
| P4 operations and Bark | Delivered in automated coverage | Health, metrics, integrity, retention, asynchronous Bark, and failed-Bark upload isolation are merged. NAS upgrade/recovery drills remain P5 service rows. |
| P5 isolated regression matrix | Checklist delivered | Use disposable roots and profiles only. No Odin2, NAS, or sole live save participates in first validation. Each physical-device row is pending evidence. |

### Release Rules

1. `game-save` releases publish a versioned image, release notes, and a rollback-ready data-directory
   checklist. Updating a running NAS container remains a separate, explicit operation.
2. OpenSave release automation currently produces Windows artifacts only. Android may compile and run unit
   checks in CI, but no APK is published or installed until the Android P5 rows have passed.
3. Eden desktop and Android builds are reviewed separately. A native C++ or Android build environment
   failure blocks the corresponding artifact; it does not weaken the no-overwrite rule.
4. Every client release includes its protocol fallback boundary: native typed scopes may fall back only on
   a route-level `404`, never after authentication, integrity, or CAS failure.

## Delivery Phases

### P0: Contract And Release Baseline

Status: delivered in source; release and deployment remain separate operations.

- Publish the component responsibilities, immutable-save rules, compatibility matrix, and release gates.
- Keep `game-save`, Eden, and OpenSave releases independently versioned. A client feature never implies an
  unreviewed server-image replacement.
- Maintain a short release checklist: automated checks, published artifact/image when applicable, upgrade
  notes, and one isolated end-to-end test.

Acceptance: the documented contract is linked from the server repository, and every subsequent PR references
the affected phase and acceptance case.

### P1: Rollback And Recovery Protection

Status: the server-side v2 write protection and the Eden recovery baseline are released.

- `game-save`: reject protocol-v1 Eden writes at blob, normal snapshot, and conflict-snapshot endpoints.
- Eden: never reuse a restore backup directory; quarantine missing-parent, cyclic, and descendants of unsafe
  pending chains instead of uploading them.
- OpenSave: retain a local divergent archive as a conflict candidate until the user chooses an explicit
  restore/replace action. This follows the same lineage rule but remains in OpenSave's typed namespaces.

Acceptance: unit coverage proves every Eden write endpoint rejects old protocol clients; release code preserves
unique recovery copies and quarantines unsafe chains; an isolated two-device stale-write scenario leaves the
cloud head unchanged and creates a conflict record.

### P2: Sync State And Conflict Visibility

Status: server and client source delivery is complete; the isolated P5 device matrix remains the release gate.
`game-save v1.4.0` is the first delivery and `v1.4.1` hardens read-only behavior:
`GET /v1/scopes/{id}/status`
returns the CAS-authoritative head, all recoverable versions, retained conflicts, source device identity,
and server receive times. It returns a successful empty state for a new scope and does not record a
heartbeat or modify any save metadata. `created_at` remains display metadata only; no client may use it
to choose a winning version. `v1.4.1` applies the same no-heartbeat rule to OpenSave's native archive,
scope, and head reads.

- `game-save` dashboard exposes current head ID, content digest, source device, last confirmed sync, pending
  conflict copies, available recoverable versions, and verification result for each scope.
- Eden displays per-title state: up to date, upload pending, download pending, conflict, offline/error, or
  restore backup present. It must show the source device and snapshot identity, not label a newer file by time.
- OpenSave displays the same state for each archive, including namespace/type and cloud version. It must not
  replace a local archive while a conflict remains unresolved.

Acceptance: a tester can identify the authoritative cloud head, its device source, and every recoverable
alternative without inspecting server files or guessing from timestamps.

#### P2 Completion Checklist

- [x] `game-save`: publish typed CAS scope state and a read-only Eden status endpoint.
- [x] `game-save`: ensure every native OpenSave read and Eden status read avoids device-heartbeat writes.
- [x] Eden: show title, local confirmed head, authoritative cloud head, source device, conflicts, pending
  work, and recovery copies in a view that cannot upload, download, or restore.
- [x] OpenSave: expose the same fields per `{type}/{gameId}` through its local API and Cloud tab.
- [x] OpenSave: reject an ordinary restore while its native scope still has a retained conflict.
- [ ] Regression: prove an isolated two-device stale-write case keeps the original head and lists the
  divergent candidate as a conflict on both clients.

#### P2 Delivery Order And Native OpenSave Contract

Status: immutable typed OpenSave scopes shipped in `v1.3.0`. OpenSave consumes that protocol in its
follow-up release; Eden consumes the generic read-only status endpoint introduced by `v1.4.0`.

1. `game-save` stores each OpenSave archive immutably below its existing typed namespace. Retrying the same
   archive bytes is idempotent; a same-name upload with different bytes is rejected.
2. A native OpenSave scope is exactly `{type}/{gameId}`. `type` is `pc`, `switch`, or `emulator-<id>` and
   never crosses into another namespace.
3. A client uploads an archive, then submits `id`, `archive`, `parent`, `contentDigest`, and `createdAt` to
   `PUT /v1/opensave/scopes/{type}/{gameId}/snapshots/{id}` with `If-Match: "<confirmed-head>"`. The server
   verifies the archive digest and advances the head only when both the header and parent equal its current
   head. `If-Match: ""` creates the first head.
4. A rejected candidate is retained with
   `PUT /v1/opensave/scopes/{type}/{gameId}/conflicts/{id}`. It never changes the head and remains a
   recoverable alternative. Archives named by a scope state cannot be removed through the native file API.
5. `GET /v1/opensave/scopes/{type}/{gameId}` returns an empty state for a new scope and otherwise returns
   the authoritative head, every snapshot, and conflict copies. `GET .../head` returns only the current head
   with its ETag. These APIs allow a new client to detect CAS support without treating a missing first head as
   a legacy-server failure.
6. OpenSave desktop and Android will prefer this contract. They retain the previous ZIP-head protocol only
   when a server returns a route-level `404`, so users can upgrade clients before the server image without
   losing compatibility.

### P3: Save Discovery And Typed Sources

Status: source delivery is complete; Root/SAF and emulator discovery remain subject to P5 physical-device
validation.

- OpenSave filters selection and library views by `pc`, `switch`, and `emulator-<id>`; the type is persisted
  in native storage and shown in every backup/restore view.
- Desktop discovery continues to use its existing save rules, with emulator roots selected separately rather
  than folded into PC-game results.
- Android supports explicit sources for public storage, SAF-selected document trees, and rooted private roots.
  Source metadata records package/emulator identifier, title ID when available, display name, icon, and access
  capability. GameHub and Eden are adapters, not assumptions that every Android device has root access.

Acceptance: independently selected PC, Switch, Eden, and another emulator test roots appear in the correct
category, keep their icons/names where metadata is available, and upload to distinct native namespaces.

### P4: Operations, Backup, And Upgrade Reliability

Status: implementation and automated coverage are complete. A production NAS deployment, image upgrade, or
recovery drill remains a separate P5 service operation against a disposable data directory.

- Add health, integrity, storage, retention, and last-successful-sync signals to the dashboard.
- Keep Bark notifications focused on actionable conflict, backup failure, integrity failure, and optionally
  successful upload events. A notification failure must never block save processing.
- Document NAS backup of the complete data directory, fixed Docker image tags, rollback procedure, and a
  no-downtime migration check. Do not run two containers against one data directory.

Acceptance: a clean server restore preserves heads, conflicts, and OpenSave namespaces; a simulated Bark
failure leaves a successful upload intact; an image upgrade is verified through health and authenticated reads
before any client is pointed at it.

### P5: Isolated End-To-End Regression Matrix

Status: checklist delivered; physical-device execution is intentionally pending a disposable test
profile/root. The exact fixture, expected CAS states, evidence rules, and stop conditions are in
[`ISOLATED_REGRESSION_MATRIX.md`](ISOLATED_REGRESSION_MATRIX.md).

- Windows: native OpenSave PC and emulator test directories, offline queue, normal shutdown, forced process
  exit, divergence, restore, and re-open checks.
- Odin2/Android: Root and non-root/SAF test roots, source capability labels, background/foreground retry, and
  no writes outside the selected test root.
- Eden desktop and Android: normal stop, stale local save, disconnected upload, conflict selection, and
  recovery-copy inspection against the same test title/profile.
- Service: restart during upload, retention, dashboard visibility, Bark event delivery, and image rollback.

Acceptance: every matrix row has a reproducible script or checklist, sanitized evidence, and a known expected
state. Only after that may a user opt in to a separately backed-up live save migration.

## Change Control

For each phase, make a focused PR, pass its CI, review its compatibility impact, then merge and publish the
artifact required by that component. Server, desktop, and Android changes may progress in parallel, but a
protocol or restore behavior change cannot be called delivered until its dependent client release exists.
