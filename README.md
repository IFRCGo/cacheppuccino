# cacheppuccino ☕

**cacheppuccino** is a lightweight translation caching service written in Go for GO.

It periodically downloads an XLSX export from a translation service, holds it in memory,
and exposes an HTTP API to fetch translations by page and language.

## Features

- XLSX import from external translation service
- Mock mode: pull the XLSX from a plain URL instead of the API (`TRANSLATION_SOURCE=url`)
- Translations served from an immutable in-memory snapshot; no database, no I/O per request
- Stateless replicas: a new pod hydrates from a sibling rather than from upstream
- A single primary, elected through a Kubernetes Lease, does all upstream pulling
- Fetch translations by page(s) + language
- ETag / If-None-Match support on `/strings`
- Consistent JSON response envelope
- OpenAPI 3 schema generation (via kin-openapi)
- Health endpoints separated by consumer, plus a fleet view and an uptime-check endpoint


## Architecture

- Go 1.23, no database
- Each application's translations live in an immutable `Snapshot` behind an atomic pointer,
  so an import swaps a pointer instead of blocking readers
- Distroless runtime image, non-root, read-only root filesystem
- kin-openapi for schema generation

### Where a pod's data comes from

In order of preference, cheapest first:

```
boot ─> emptyDir cache ─> peer hydration ─> upstream pull
        (container       (any ready peer,   (last resort; the
         restarts)        in-cluster)        primary only, in
                                             steady state)
```

Only the **primary** pulls from upstream, so upstream load does not scale with the replica
count. The primary is whichever pod holds a `coordination.k8s.io/v1` Lease; if it stops
renewing, another pod takes over after `LEASE_DURATION`. Acquiring the lease is a
compare-and-swap on `resourceVersion`, so the API server rejects a racing write and two
pods cannot both win.

Followers poll the primary every `PEER_POLL_INTERVAL` and fetch a snapshot only when the
hash differs. The XLSX itself is transferred and verified against the advertised SHA-256,
so a follower's ETag is provably derived from the bytes upstream produced. A pod never
adopts a version older than the one it already serves.

**Losing the API server degrades freshness, never availability.** A pod that cannot reach
it keeps serving from memory; it just stops pulling. Nothing falls back automatically, so
`/monitor` is what makes that visible.


## API Overview

All endpoints return a consistent envelope.

### Success

```json
{
  "ok": true,
  "data": { ... }
}
```

### Error

```json
{
  "ok": false,
  "error": {
    "code": "bad_request",
    "message": "missing required query param: lang"
  }
}
```


## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/strings` | Get strings by page(s) and language |
| GET | `/healthz` | Liveness |
| GET | `/readyz` | Readiness |
| GET | `/status` | This pod's status and per-application sync state |
| GET | `/cluster` | Fleet view: every pod, the primary, and whether replicas agree |
| GET | `/monitor` | Fleet health for an external uptime check |
| GET | `/openapi.json` | OpenAPI 3 schema |

Pod-to-pod endpoints (`/internal/peer`, `/internal/snapshot`) are served on
`INTERNAL_LISTEN_ADDR`, which is never published through the Service or the ingress.


## Example Usage

### Fetch translations

```bash
curl "http://localhost:8080/strings?page=home&page=about&lang=en"
```

Or CSV style:

```bash
curl "http://localhost:8080/strings?pages=home,about&lang=en"
```

An `app` parameter selects the application; it defaults to `default`, the single
application the current configuration describes.


## Health checks

Three consumers ask three different questions, and they are deliberately not the same
endpoint. Conflating them is what produced an incident where every probe was green while
`/strings` was failing.

| Endpoint | Consumer | Question | May fail on a dependency? |
|---|---|---|---|
| `/healthz` | kubelet liveness | Is this process wedged? | Never, except a sync loop that has stopped ticking |
| `/readyz` | kubelet readiness | Can *this pod* serve `/strings`? | Only on its own snapshot |
| `/status` | humans | What is *this pod* doing? | No, always 200 |
| `/cluster` | humans | What is the *fleet* doing? | No, always 200 |
| `/monitor` | uptime check | Is the *fleet* healthy? | Yes — that is its job |

`/readyz` reports whether this pod holds servable translations, which is the same data
`/strings` reads rather than a proxy for it. Nothing about the Lease, the API server,
peers, or upstream may influence it: routing those into readiness would make every pod
unready at once and turn stale data into a total outage.

`/monitor` answers 503 when a fleet alarm fires and names which one in the body. Alarms
require their condition to hold for a configured duration, because brief divergence while
a snapshot propagates is normal. Point the external uptime check here.

`/cluster` reports `in_sync`, a direct answer to "are all replicas serving the same data".
It is cached for `CLUSTER_CACHE_TTL` so a public request cannot fan out to every pod on
every hit.


## Sync Behavior

On startup a pod loads its local cache and becomes ready immediately if that cache is
usable, then converges with the fleet. With no usable cache it tries peers, and only then
upstream, retrying on a short backoff until it first succeeds — waiting a whole
`PULL_INTERVAL` after a transient failure would stall a rollout.

Each import fully replaces the snapshot, so rows removed from the XLSX disappear. The
service avoids re-importing unchanged files by hashing the downloaded content, and a file
that parses to nothing is rejected rather than replacing good data.

### XLSX format

The export must contain a sheet named `Translations` (falls back to the first sheet), with
a header row of exactly `page | key | <lang> | <lang> | ...` (case-insensitive). Files with
other header names (e.g. `Namespace`) are rejected.

### Response caching

`/strings` responses carry an `ETag` derived from the import hash and
`Cache-Control: public, max-age=60`. Requests with a matching `If-None-Match` get `304 Not
Modified`. The ETag and the body come from a single snapshot load, so they can never
describe different imports.

## Mock mode (`TRANSLATION_SOURCE=url`)

For QA/alpha instances the service can pull the XLSX from any plain HTTP(S) URL
instead of the IFRC translation API:

```env
TRANSLATION_SOURCE=url
TRANSLATION_XLSX_URL=https://example.com/ifrc-go/translations.xlsx
```

- The file must be in the exact format the translation service exports
  (see "XLSX format" above; `Namespace` headers are rejected).
- Public URLs, Azure Blob SAS URLs, and S3 presigned URLs all work; no
  auth headers are sent.
- The regular pull loop applies: updates to the hosted file show up within
  `PULL_INTERVAL` (alpha uses `1m`); unchanged files are skipped by hash.
- Check `GET /status` to debug a broken file: it reports `source`,
  `last_pull_error` (cleared on success), and the imported row count.
- In `url` mode the `TRANSLATION_BASE_URL`, `TRANSLATION_APPLICATION_ID`,
  and `TRANSLATION_API_KEY` variables are ignored.


## Environment Variables

Defaults for every one of these are set in `helm/values.yaml`.

### Translation source

| Variable | Required | Description |
|----------|----------|------------|
| `TRANSLATION_SOURCE` | No | `api` (default) or `url` (mock mode) |
| `TRANSLATION_BASE_URL` | api mode | Base URL of translation service |
| `TRANSLATION_APPLICATION_ID` | api mode | Translation application ID |
| `TRANSLATION_API_KEY` | api mode | Sent as `X-API-KEY` header |
| `TRANSLATION_XLSX_URL` | url mode | HTTP(S) URL of the mock XLSX file |

### Listeners and logging

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | Public API |
| `INTERNAL_LISTEN_ADDR` | `:8081` | Pod-to-pod endpoints; never exposed publicly |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

### Local cache

| Variable | Default | Description |
|----------|---------|-------------|
| `CACHE_DIR` | `/cache` | Snapshot cache; empty disables it |
| `MAX_CACHE_AGE` | `24h` | Older entries are discarded rather than served |

### Peers

| Variable | Default | Description |
|----------|---------|-------------|
| `PEER_SERVICE` | (unset) | Headless Service DNS name; unset disables peer hydration |
| `PEER_POLL_INTERVAL` | `5s` | How quickly a follower notices new content |
| `PEER_TIMEOUT` | `3s` | Per-request timeout when talking to a peer |

### Leader election

| Variable | Default | Description |
|----------|---------|-------------|
| `LEADER_ELECTION` | `off` | `lease` or `off`; `off` makes the process unconditionally primary |
| `LEASE_NAME` | `cacheppuccino` | Lease object name |
| `LEASE_DURATION` | `15s` | How long a lease survives without renewal |
| `LEASE_RENEW_INTERVAL` | `5s` | Must be shorter than `LEASE_DURATION` |

### Pulling

| Variable | Default | Description |
|----------|---------|-------------|
| `PULL_INTERVAL` | `10m` | Steady-state interval, with up to 10% jitter |
| `PULL_CONCURRENCY` | `4` | Applications pulled in parallel |
| `HTTP_TIMEOUT` | `30s` | Upstream request timeout |
| `INITIAL_PULL_DEADLINE` | `45s` | `0` means no deadline |
| `INITIAL_PULL_BACKOFF_MIN` | `2s` | Retry floor before the first success |
| `INITIAL_PULL_BACKOFF_MAX` | `30s` | Retry ceiling before the first success |

### Alarms and fleet view

| Variable | Default | Description |
|----------|---------|-------------|
| `ALARM_NO_PRIMARY` | `5m` | Nobody holds the lease |
| `ALARM_SNAPSHOT_AGE` | `4 × PULL_INTERVAL` | No pod has pulled successfully in this long |
| `ALARM_DIVERGENCE` | `2m` | Replicas hold different snapshots |
| `ALARM_PEER_UNREADY` | `2m` | A pod is unready or unreachable |
| `ALARM_PULL_FAILURES` | `3` | Consecutive failed pulls |
| `CLUSTER_CACHE_TTL` | `5s` | How long `/cluster` and `/monitor` reuse a fleet view |

`POD_NAME`, `POD_NAMESPACE` and `POD_IP` come from the downward API; the chart sets them.


## Running with Docker

Create a `.env` file:

```env
TRANSLATION_BASE_URL=https://example.com
TRANSLATION_APPLICATION_ID=your-app-id
TRANSLATION_API_KEY=your-api-key
```

Then run:

```bash
docker compose up --build
```

A single container has no siblings and no API server, so it runs with
`LEADER_ELECTION=off` and no peer service: it is always the primary and pulls upstream
itself. The snapshot cache lives in the `cacheppuccino_cache` volume.

To reset it:

```bash
docker compose down -v
```


## OpenAPI Schema

Fetch:

```bash
curl http://localhost:8080/openapi.json
```

The schema is generated dynamically using kin-openapi based on Go structs.

### Generate `openapi.json` using commandline

```bash
go run . --schema
```


## Deployment

See `helm/README.md` for the topology, the resources the chart creates, and why there is
no PersistentVolume.
