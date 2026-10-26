# Game Save

The cross-component delivery contract and release gates are in
[`docs/INTEGRATION_ROADMAP.md`](docs/INTEGRATION_ROADMAP.md).

`game-save` is a self-hosted private backend for game-save synchronization. It
is the standalone successor to `eden-syncd`: the existing Eden protocol remains
available, while OpenSave uses a separate, typed archive API. The service never
interprets game-save contents.

The next standalone release is `v1.4.1`; its matching image will be
`jun9100/game-save:v1.4.1` for `linux/amd64` and `linux/arm64`.

## Client Protocols

### Eden

Eden continues to use its existing authenticated `/v1` object and snapshot-head
protocol. Existing Eden client URLs and sync keys do not need to change when an
`eden-syncd` container is replaced by `game-save`.

`GET /v1/scopes/{scope}/status` is a read-only status endpoint for newer Eden
clients. It returns the CAS-authoritative head, every recoverable version,
retained conflict copies, source device identity, and server receive times. A
new scope returns an empty state with HTTP 200. The request neither records a
device heartbeat nor changes save metadata; client-side state inspection must
not trigger an upload, download, or restore.

### OpenSave Native API

The matching OpenSave client integration uses the same private service URL and
sync key, without a WebDAV URL or Basic Auth credentials. Archives use a
mandatory namespace:

- `pc`
- `switch`
- `emulator-<name>` for future emulator types

The native endpoints are:

```text
GET    /v1/opensave/files
PUT    /v1/opensave/files/{type}/{name}
GET    /v1/opensave/files/{type}/{name}
DELETE /v1/opensave/files/{type}/{name}
GET    /v1/opensave/scopes/{type}/{gameId}
GET    /v1/opensave/scopes/{type}/{gameId}/head
PUT    /v1/opensave/scopes/{type}/{gameId}/snapshots/{snapshotId}
PUT    /v1/opensave/scopes/{type}/{gameId}/conflicts/{snapshotId}
```

They require `Authorization: Bearer <GAME_SAVE_TOKEN>`. Native archives live at
`opensave-native/<type>/` and cannot collide with Eden encrypted objects or the
legacy WebDAV collection. The first upload creates an immutable archive; a
retry with the same bytes is idempotent, while different bytes under the same
name return `409 Conflict`. A client advances a typed `{type}/{gameId}` cloud
head only through the snapshot endpoint with an `If-Match` header for its
confirmed parent. A rejected branch is sent to the conflict endpoint and
remains recoverable without changing the head. A matching OpenSave client build
is required before selecting this provider.

Native OpenSave `GET` operations are authenticated but read-only: listing
archives or checking a typed scope/head never writes a device heartbeat.

### OpenSave WebDAV Compatibility

Older OpenSave builds can continue to use `https://host:port/opensave/` with
Basic Auth. Set both `GAME_SAVE_OPENSAVE_USERNAME` and
`GAME_SAVE_OPENSAVE_PASSWORD` to enable it. WebDAV files remain in the separate
`opensave/` directory and do not share native OpenSave storage.

## Compatibility With `eden-syncd`

`game-save` is an in-place protocol-compatible replacement:

- New environment variables use `GAME_SAVE_*`.
- The corresponding `EDEN_SYNCD_*` variables remain accepted as a temporary
  fallback, but new deployments should use the new names.
- The old `X-Eden-Admin` dashboard write header is still accepted; new clients
  send `X-Game-Save-Admin`.
- Existing state files, encrypted blobs, dashboard metadata, and WebDAV files
  are read without a data migration.

The persistent data directory contains `objects/`, `state.json`,
`dashboard.json`, `settings.json`, `events.json`, `opensave/`, and (after a
native OpenSave upload) `opensave-native/`, `opensave-native.json`, plus
`opensave-native-state.json`. The JSON catalog and state contain audit metadata,
content digests, source devices, parent lineage, current heads, and retained
conflicts; they never contain save contents. Back up the entire directory.

## Configuration

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--token` | `GAME_SAVE_TOKEN` | required |
| `--admin-token` | `GAME_SAVE_ADMIN_TOKEN` | dashboard disabled |
| `--data-dir` | `GAME_SAVE_DATA` | `./data` (`/var/lib/game-save` in Docker) |
| `--listen` | `GAME_SAVE_LISTEN` | `:8787` |
| `--opensave-username` | `GAME_SAVE_OPENSAVE_USERNAME` | WebDAV disabled unless paired with password |
| `--opensave-password` | `GAME_SAVE_OPENSAVE_PASSWORD` | WebDAV disabled unless paired with username |
| `--max-object-mib` | `GAME_SAVE_MAX_OBJECT_MIB` | `512` |
| `--keep-snapshots` | `GAME_SAVE_KEEP_SNAPSHOTS` | `0` (unlimited) |
| `--keep-days` | `GAME_SAVE_KEEP_DAYS` | `0` (unlimited) |
| `--maintenance-interval` | `GAME_SAVE_MAINTENANCE_INTERVAL` | `6h` |
| `--bark-url` | `GAME_SAVE_BARK_URL` | disabled |
| `--bark-group` | `GAME_SAVE_BARK_GROUP` | `game-save` |
| `--bark-ttl` | `GAME_SAVE_BARK_TTL` | `600` seconds |
| `--bark-events` | `GAME_SAVE_BARK_EVENTS` | `conflict` |
| `--bark-timeout` | `GAME_SAVE_BARK_TIMEOUT` | `5s` |
| `--log-format` | `GAME_SAVE_LOG_FORMAT` | `text` or `json` |

`game-save healthcheck` probes `GET /healthz`. `game-save --version` reports
the build version.

## Run Locally

```sh
export GAME_SAVE_TOKEN='replace-with-a-long-random-sync-token'
export GAME_SAVE_ADMIN_TOKEN='replace-with-a-different-admin-token'
go run . --data-dir ./data --listen 127.0.0.1:8787
```

The health endpoint is unauthenticated. All `/v1` endpoints require the sync
token. Set an administrator token different from the sync token to enable the
private dashboard at `/admin/`.

Eden save writes require Save Sync protocol v2. Upgrade every Eden client
before deploying the `game-save` release that enables this requirement: older
clients can still read, but cannot upload a stale local save as a new cloud
head. OpenSave's separate native and WebDAV endpoints are unaffected.

When current clients detect divergent local files, they preserve the candidate
as a fixed **conflict copy** instead of advancing the cloud head. It appears in
the dashboard's snapshot history and can be inspected, downloaded, or made
current with the existing restore action. Conflict copies are never removed by
the retention policy; delete one explicitly after reviewing it.

## Preventing Save Rollback

`game-save` does not use wall-clock time as the authority for a save. A client
may only advance a cloud head when its local files match the content digest of
the head it previously confirmed. A stale local directory, manual replacement,
or interrupted session is retained as a conflict copy instead. During the v2
client migration, an existing local directory must first match a read-only
download of the cloud head before it becomes trusted.

For reliable cross-device play:

1. Upgrade every Eden client to Save Sync protocol v2 before upgrading the
   server to `v1.1.0` or later.
2. Play one title on only one device at a time. Stop emulation normally and
   keep that device online until it returns to the game library before opening
   the same title elsewhere.
3. After a forced process kill, power loss, or network outage, reopen Eden on
   the device that was playing first. It retries the locally persisted snapshot
   before continuing that title on another device.
4. If a conflict copy appears, stop both sessions, compare the actual game
   state, and use the dashboard's restore action once. The matching device
   recognizes the selected cloud content and resumes ordinary synchronization.

## Bark Notifications

Set `GAME_SAVE_BARK_URL` to the Bark base URL including its device path. A
complete URL copied from a Bark `curl` example is also accepted: its final
`/title/body` segments and query string are removed automatically. For example,
`https://bark.example.com/device-key/title/body` becomes
`https://bark.example.com/device-key`.

`game-save` calls Bark asynchronously after its local activity event is
persisted. A Bark outage never blocks an upload, conflict response, dashboard
action, or maintenance task. The default only notifies `conflict`, which is the
actionable condition where a device attempted to advance a stale cloud head and
its local snapshot was retained. Set `GAME_SAVE_BARK_EVENTS=conflict,upload`
to also receive successful uploads. Duplicate events for the same game, device,
and cloud head are coalesced for five minutes.

## Docker And NAS Deployment

Copy [`.env.example`](.env.example) to `.env`, replace placeholders, then use a
fixed release tag rather than `latest`:

```sh
docker compose -f compose.nas.yaml up -d
```

`compose.nas.yaml` defaults to `jun9100/game-save:v1.4.1`, maps the service to
port `8788`, and stores data at `/volume1/docker/game-save/data`. Set `PUID` and
`PGID` to the owner of that directory. The generic default binds to loopback;
choose the NAS private address only when the LAN firewall protects the port.

For Synology Container Manager, use
[`compose.synology.yaml`](compose.synology.yaml). It avoids Compose variable
interpolation that some DSM releases reject. Change its host path and `user`
field for the NAS before deployment.

## Safe Migration From `eden-syncd`

1. Back up the old data directory while the old container is still healthy.
2. Stop the old `eden-syncd` container, but do not remove it or its data.
3. Copy the old data directory to the new `game-save` project directory.
4. Create a new `game-save` container with the same host port and mount the new
   copy at `/var/lib/game-save`.
5. Verify `GET /healthz`, an existing authenticated Eden `/v1` request, and the
   OpenSave route before treating the migration as complete.

The old container can remain stopped as a rollback target. Do not run old and
new containers against the same data directory at the same time.

## Dashboard

Set `GAME_SAVE_ADMIN_TOKEN` to enable `/admin/`. The dashboard uses an HttpOnly,
SameSite=Strict session cookie. State-changing dashboard API requests require
`X-Game-Save-Admin: 1`. Keep the dashboard on a private network and use HTTPS
for any remote access.

## Development

```sh
go test ./...
go vet ./...
docker build --build-arg VERSION=v1.4.1 -t game-save:v1.4.1 .
```

The GitHub Actions release workflow publishes multi-architecture Docker Hub
images only for `v*` Git tags or an explicit manual dispatch.
