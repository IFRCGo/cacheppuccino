# cacheppuccino ☕

**cacheppuccino** is a lightweight translation caching service written in Go for GO.

It periodically downloads an XLSX export from a translation service, stores the data in SQLite, and exposes an HTTP API to fetch translations by page and language.

## Features

- XLSX import from external translation service
- Periodic background sync
- SQLite-backed cache
- Fetch translations by page(s) + language
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

1. Performs an initial XLSX pull.
2. If successful → service becomes ready.
3. Periodically refreshes based on `PULL_INTERVAL`.

The service avoids re-importing unchanged XLSX files by hashing the downloaded content.


## Environment Variables

| Variable | Required | Description |
|----------|----------|------------|
| `TRANSLATION_BASE_URL` | Yes | Base URL of translation service |
| `TRANSLATION_APPLICATION_ID` | Yes | Translation application ID |
| `TRANSLATION_API_KEY` | No | Sent as `X-API-KEY` header |
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
- `GET /readyz` → initial sync completed
- Docker healthcheck uses internal `--healthcheck` flag


## OpenAPI Schema

Fetch:

```bash
curl http://localhost:8080/openapi.json
```

The schema is generated dynamically using kin-openapi based on Go structs.


## Database

- SQLite
- WAL mode enabled
- Uses `mode=rwc`
- Indexed by `(page, lang)`
- Metadata table stores:
  - `last_pull_rfc3339`
  - `last_xlsx_sha256`

## Development

Run locally:

```bash
go mod tidy
go run .
```

Build binary:

```bash
go build -o cacheppuccino
```
