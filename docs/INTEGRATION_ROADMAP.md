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
| `game-save` | `v1.1.0` | Eden protocol v2 is required for all Eden writes; native OpenSave API has typed storage. | P1 server-side protection is already in this release; a new image is only tagged when server behavior changes. |
| Eden | `v2026.10.25-1` | Uses the v2 protocol, records confirmed content state, preserves restore copies, quarantines unsafe pending chains, and creates conflicts instead of overwriting a newer cloud head. | Add per-title sync-state visibility and isolated regression coverage. |
| OpenSave desktop | `v2.4.5` | Uses the native provider, with PC/Switch/emulator namespaces isolated. | Add per-save sync status and a manifest-based restore/conflict contract. |
| OpenSave Android | `0.3.15 (20)` | Supports selected Root and SAF roots, type filters, icons/names, and batch/automatic backups. | Add source-capability state, native sync status, and isolated device regression coverage. |

Older Eden clients remain readable but are intentionally unable to write to a v2 `game-save` service. All
devices sharing an Eden save must be upgraded before they are used interchangeably.

## Delivery Phases

### P0: Contract And Release Baseline

Status: in progress with this document.

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

- `game-save` dashboard exposes current head ID, content digest, source device, last confirmed sync, pending
  conflict copies, available recoverable versions, and verification result for each scope.
- Eden displays per-title state: up to date, upload pending, download pending, conflict, offline/error, or
  restore backup present. It must show the source device and snapshot identity, not label a newer file by time.
- OpenSave displays the same state for each archive, including namespace/type and cloud version. It must not
  replace a local archive while a conflict remains unresolved.

Acceptance: a tester can identify the authoritative cloud head, its device source, and every recoverable
alternative without inspecting server files or guessing from timestamps.

### P3: Save Discovery And Typed Sources

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

- Add health, integrity, storage, retention, and last-successful-sync signals to the dashboard.
- Keep Bark notifications focused on actionable conflict, backup failure, integrity failure, and optionally
  successful upload events. A notification failure must never block save processing.
- Document NAS backup of the complete data directory, fixed Docker image tags, rollback procedure, and a
  no-downtime migration check. Do not run two containers against one data directory.

Acceptance: a clean server restore preserves heads, conflicts, and OpenSave namespaces; a simulated Bark
failure leaves a successful upload intact; an image upgrade is verified through health and authenticated reads
before any client is pointed at it.

### P5: Isolated End-To-End Regression Matrix

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
