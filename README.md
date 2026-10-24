# Game Save

`game-save` is a self-hosted private backend for game-save synchronization. It
is the standalone successor to `eden-syncd`: the existing Eden protocol remains
available, while OpenSave uses a separate, typed archive API. The service never
interprets game-save contents.

The first standalone release is `v1.0.0` and is published as
`jun9100/game-save:v1.0.0` for `linux/amd64` and `linux/arm64`.

## Client Protocols

### Eden

Eden continues to use its existing authenticated `/v1` object and snapshot-head
protocol. Existing Eden client URLs and sync keys do not need to change when an
`eden-syncd` container is replaced by `game-save`.

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
```

They require `Authorization: Bearer <GAME_SAVE_TOKEN>`. Native archives live at
`opensave-native/<type>/` and cannot collide with Eden encrypted objects or the
legacy WebDAV collection. A matching OpenSave client build is required before
selecting this provider.

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
native OpenSave upload) `opensave-native/`. Back up the entire directory.

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

## Docker And NAS Deployment

Copy [`.env.example`](.env.example) to `.env`, replace placeholders, then use a
fixed release tag rather than `latest`:

```sh
docker compose -f compose.nas.yaml up -d
```

`compose.nas.yaml` defaults to `jun9100/game-save:v1.0.0`, maps the service to
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
docker build --build-arg VERSION=v1.0.0 -t game-save:v1.0.0 .
```

The GitHub Actions release workflow publishes multi-architecture Docker Hub
images only for `v*` Git tags or an explicit manual dispatch.
