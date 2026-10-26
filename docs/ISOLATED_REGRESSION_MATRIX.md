# Isolated Save Regression Matrix

This is the P5 acceptance checklist for `game-save`, OpenSave, and Eden. It is
deliberately designed around disposable test data. Do not substitute a player's
only game save, an active Eden profile, a production NAS data directory, or an
existing cloud scope for any fixture below.

## Rules Before Starting

1. Create one empty test root per client and give it a unique marker, such as
   `GAME-SAVE-REGRESSION-2026-10-26`. A test root must not contain a real game's
   save directory.
2. Use a local `game-save` process or a separate disposable container/data
   volume. It must have a new test token and a port distinct from the NAS.
3. Use an independently created Eden user/profile and a non-production test
   title. Do not point a test client at a user's existing title/account scope.
4. Record the cloud head ID, source device identity, and retained conflict IDs.
   Never declare a snapshot newer because its filesystem time is newer.
5. Stop immediately if an app proposes a source or restore target outside the
   planned test root. Preserve the test data and record the unexpected path;
   do not retry against a live root.
6. Evidence must not include sync tokens, Bark device URLs, public addresses,
   ROM content, keys, or save contents. Redact those values before attaching a
   result to a pull request or issue.

## Shared Fixture

Create the following distinct, disposable roots. The content can be small text
files; no game binary is needed.

| Fixture | Example logical identity | Required marker |
| --- | --- | --- |
| PC save | `pc/regression-pc` | `PC-REV-A` |
| Switch emulator save | `switch/010000000000BEEF` | `SWITCH-REV-A` |
| Another emulator save | `emulator-ppsspp/regression-ppsspp` | `PSP-REV-A` |
| Android SAF source | SAF-selected `OpenSave-Regression` tree | `SAF-REV-A` |
| Android Root source | `/data/local/tmp/opensave-regression` | `ROOT-REV-A` |

The Switch and emulator roots must use the directory layout recognized by the
tested client. The point is an isolated title ID and source root, not a real
commercial title or an existing emulator account.

Start the service against a temporary data directory. The following is a local
example only; run it in one terminal and run the later `curl` requests from a
second terminal. Set the token in the shell rather than copying it into a
document or terminal history:

```sh
export TEST_DATA="$(mktemp -d)"
export GAME_SAVE_TOKEN="$(openssl rand -hex 32)"
export GAME_SAVE_ADMIN_TOKEN="$(openssl rand -hex 32)"
go run . --data-dir "$TEST_DATA" --listen 127.0.0.1:18787
```

Verify the unauthenticated health route and an authenticated empty OpenSave
scope before pointing any client at the service:

```sh
curl -fsS http://127.0.0.1:18787/healthz
curl -fsS -H "Authorization: Bearer $GAME_SAVE_TOKEN" \
  http://127.0.0.1:18787/v1/opensave/scopes/switch/010000000000BEEF
```

The first request returns `{"status":"ok"}`. The second returns a successful
empty scope (`head: null`), proving that the native CAS route exists without
creating a device heartbeat or archive.

## Automated Gate

Run these checks before a manual device row. They are fixture-only and do not
contact a user service.

| ID | Component | Check | Expected result |
| --- | --- | --- | --- |
| AUTO-1 | game-save | `go test ./...` | CAS, restart, conflict retention, integrity, metrics, and Bark failure checks pass. |
| AUTO-2 | OpenSave desktop | `go test ./...` | Native typed scopes and conflict/recovery behavior pass. |
| AUTO-3 | OpenSave Android | `cd android && ./gradlew --no-daemon :app:testDebugUnitTest :app:lintDebug :app:assembleDebug` | Capability labels, source isolation, and Android debug build pass. |
| AUTO-4 | Eden core | Build `core` from a clean CMake build directory. | The read-only title-status implementation compiles. |
| AUTO-5 | Eden Android | Build the release APK in CI or locally. | The status dialog and JNI boundary compile. |

Record the commit IDs and command exit codes, but not environment variables or
credentials.

## Service Rows

| ID | Procedure | Expected state |
| --- | --- | --- |
| GS-01 | Start an empty local service, run the two shared-fixture requests, then restart it with the same temporary data directory. | Health remains successful; the empty scope remains empty; no device is recorded by either read. |
| GS-02 | Upload a test archive, publish snapshot `A` with `If-Match: ""`, restart the service, and query its typed scope head. | The head remains `A` with the same digest and type; it is not selected by timestamp. |
| GS-03 | Publish `B` as child of `A`, then attempt stale candidate `C` with parent `A`; retain `C` via the conflict endpoint. | Head remains `B`; `C` is an immutable conflict copy and retention does not remove it. |
| GS-04 | Enable Bark only for `upload` against a disposable endpoint that returns HTTP 502. Publish `A`, then read the head. | The write returns success and head `A` remains readable despite the failed notification. The automated equivalent is `TestBarkFailureDoesNotBlockOpenSaveScopeUpload`. |
| GS-05 | Copy the complete temporary data directory to a new temporary location, start a new local service on it, and query Eden/OpenSave test scopes. | Heads, conflicts, native archive state, dashboard metadata, and read-only status survive. Never run two containers against the same directory. |
| GS-06 | Use the dashboard maintenance view or its test API against the disposable service to run integrity verification and inspect metrics. | Integrity identifies only intentionally corrupted fixture data; disk, archive, and retention signals are visible. |

## Windows OpenSave Rows

| ID | Procedure | Expected state |
| --- | --- | --- |
| WIN-01 | Add only the PC, Switch, and other-emulator fixture roots. Filter each category and select all visible entries in that category. | Entries appear under `pc`, `switch`, and `emulator-ppsspp` separately; batch selection never crosses the active type. |
| WIN-02 | Back up revision `A` for every fixture, then inspect the service scopes. | Each scope is in its own typed namespace with a distinct head and digest. |
| WIN-03 | Disconnect the test service, edit only the PC fixture to revision `B`, request backup, then restore connectivity. | The client queues/retries the test archive; an unrelated emulator scope is unchanged. |
| WIN-04 | On a second disposable Windows test root, change the same scope from confirmed `A` to a divergent `C` without first accepting `B`. | The server retains `C` as a conflict; no client silently overwrites a local archive or advances the authoritative `B` head. |
| WIN-05 | Stop the app normally after a fixture update, then repeat with a forced process exit using only the test root. Reopen the same test client before another client opens that scope. | Normal shutdown completes its pending state. After forced exit, the local client either resumes/reports a pending/conflict state; it never promotes a stale root based on modification time. |
| WIN-06 | Choose an explicit restore only for a disposable root with existing marker data. | The pre-restore marker is retained in the documented recovery location before replacement; the restored digest matches the selected cloud object. |

## Android OpenSave Rows

| ID | Procedure | Expected state |
| --- | --- | --- |
| AND-01 | On a non-root device, select only the Android SAF fixture through the system picker. Reopen OpenSave after a restart. | The source shows `SAF 已授权`; read-only access allows backup only, while read/write access is required for restore. |
| AND-02 | Revoke the SAF tree permission or select a nonexistent test path, then scan. | The source displays `不可访问` with a reason; it is not reported as a source with zero saves. |
| AND-03 | On a rooted test device, grant Magisk access to OpenSave and select only `/data/local/tmp/opensave-regression`. | The source shows `仅 Root 可访问`; the test fixture is discoverable and restorable without scanning unrelated app directories. |
| AND-04 | Deny Root access and scan the same selected Root source. | The source displays `不可访问 · 需要 Root` or `Root 未授权`; no backup or restore attempt occurs. |
| AND-05 | Put the app in background during a test backup, then return foreground and inspect the library/state. | The worker retries a temporary failure; no write occurs outside the selected test root and source capability labels remain accurate. |
| AND-06 | Repeat the type-filter and batch-selection sequence using the Switch and another-emulator fixtures. | All selected entries retain their own type; Android never uploads an emulator save into `pc` or `switch` by inference. |

## Eden Desktop And Android Rows

| ID | Procedure | Expected state |
| --- | --- | --- |
| EDEN-01 | Configure only the disposable test profile/title on desktop and Android. Perform a normal emulator stop after creating revision `A`. | The title status view shows a cloud head, device source, and a state derived from confirmed lineage rather than filesystem time. |
| EDEN-02 | Let desktop publish `B` as child of `A`. On Android, keep a divergent local `C` based on `A` and attempt sync. | `B` remains the cloud head; `C` becomes a retained conflict. Both title status views expose the source device and conflict count. |
| EDEN-03 | Disconnect the service before a test update, then reconnect and refresh the status view without using restore. | The status view reports offline/pending as appropriate and remains read-only: no upload, download, restore, metadata write, or device heartbeat is created by the refresh. |
| EDEN-04 | Select an explicit restore for a test title that already contains a marker. | Eden creates a title-specific recovery copy before replacing the test save. The next status read reports the backup presence. |
| EDEN-05 | Remove a referenced object only from a copied disposable service data directory, then refresh title status. | The status is an error, never `up_to_date`; the missing object is not treated as a valid cloud recovery point. |

## Cross-Component Completion

The matrix is complete only when every applicable row records all of the
following:

1. Component version or commit, operating system, and whether the root was
   Root, SAF, or local desktop storage.
2. Test scope type/game ID, authoritative head ID, parent/head relationship,
   source device label, and conflict IDs.
3. A sanitized screenshot or JSON/status excerpt proving the expected state.
4. A statement that the selected root was disposable and that no live save or
   production data directory was mounted.
5. Result: pass, fail, skipped, or blocked, with the first failing step for a
   non-pass row.

Only after the relevant rows pass may a user separately choose to migrate a
backed-up live save. That migration is a distinct change, not a continuation of
this regression suite.
