# cacheppuccino ☕

**cacheppuccino** is a lightweight translation caching service written in Go for GO.

It periodically downloads an XLSX export from a translation service, stores the data in SQLite, and exposes an HTTP API to fetch translations by page and language.

## Features

- XLSX import from external translation service
- Mock mode: pull the XLSX from a plain URL instead of the API (`TRANSLATION_SOURCE=url`)
- Periodic background sync (full replace per import; the XLSX is the source of truth)
- SQLite-backed cache
- Fetch translations by page(s) + language
- ETag / If-None-Match support on `/strings`
- Consistent JSON response envelope
- OpenAPI 3 schema generation (via kin-openapi)
- Health + readiness endpoints


## Architecture

- Go 1.23
- SQLite (modernc driver via bun)
- kin-openapi for schema generation
- Distroless runtime image
- Periodic background puller
- Named Docker volume for persistent cache


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
| GET | `/healthz` | Liveness probe |
| GET | `/readyz` | Readiness probe |
| GET | `/status` | Sync status + hash |
| GET | `/openapi.json` | OpenAPI 3 schema |


## Example Usage

### Fetch translations

```bash
curl "http://localhost:8080/strings?page=home&page=about&lang=en"
```

Or CSV style:

```bash
curl "http://localhost:8080/strings?pages=home,about&lang=en"
```


## Sync Behavior

On startup:

1. If the SQLite cache already holds data from a previous run, `/status` reports `ready: true` immediately.
2. Performs an initial XLSX pull. A successful pull also sets `ready: true`.
3. Periodically refreshes based on `PULL_INTERVAL`.

Each import fully replaces the cached strings inside a single transaction, so rows removed
from the XLSX disappear from the cache. The service avoids re-importing unchanged XLSX
files by hashing the downloaded content.

### XLSX format

The export must contain a sheet named `Translations` (falls back to the first sheet), with
a header row of exactly `page | key | <lang> | <lang> | ...` (case-insensitive). Files with
other header names (e.g. `Namespace`) are rejected.

### Response caching

`/strings` responses carry an `ETag` derived from the last import hash and
`Cache-Control: public, max-age=60`. Requests with a matching `If-None-Match` get `304 Not Modified`.

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
  `last_pull_error` (cleared on success), and `last_import_rows`.
- In `url` mode the `TRANSLATION_BASE_URL`, `TRANSLATION_APPLICATION_ID`,
  and `TRANSLATION_API_KEY` variables are ignored.


## Environment Variables

| Variable | Required | Description |
|----------|----------|------------|
| `TRANSLATION_SOURCE` | No | `api` (default) or `url` (mock mode) |
| `TRANSLATION_BASE_URL` | api mode | Base URL of translation service |
| `TRANSLATION_APPLICATION_ID` | api mode | Translation application ID |
| `TRANSLATION_API_KEY` | api mode | Sent as `X-API-KEY` header |
| `TRANSLATION_XLSX_URL` | url mode | HTTP(S) URL of the mock XLSX file |
| `SQLITE_PATH` | No | Default: `/data/cacheppuccino.db` |
| `PULL_INTERVAL` | No | Default: `10m` |
| `HTTP_TIMEOUT` | No | Default: `30s` |
| `INITIAL_PULL_DEADLINE` | No | Default: `45s` |
| `LOG_LEVEL` | No | Default: `info` |
| `LISTEN_ADDR` | No | Default: `:8080` |


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

SQLite data is stored in a named Docker volume:

```
cacheppuccino_data
```

To reset the database:

```bash
docker compose down -v
```


## Health Checks

- `GET /healthz` → service running
- `GET /readyz` → always `200` while the process is up. Deliberately not gated on data:
  the deploy tooling restarts pods that stay unready, and with a single replica there is
  no alternative pod to route to. Whether the cache actually holds servable data is
  reported as `ready` on `GET /status`.
- Docker healthcheck uses internal `--healthcheck` flag


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


## Database

- SQLite
- WAL mode with `synchronous=NORMAL`
- Single connection (`MaxOpenConns=1`)
- Indexed by `(page, lang)`
- Metadata table stores:
  - `last_pull_rfc3339`
  - `last_xlsx_sha256`
  - `last_pull_error`
  - `last_import_rows`

## Development

Run locally:

```bash
go mod tidy
go run .
```

Run tests:

```bash
go test ./...
```

Build binary:

```bash
go build -o cacheppuccino
```
