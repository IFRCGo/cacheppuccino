# cacheppuccino

Translation caching service for IFRC GO. Periodically downloads an XLSX export from a
translation service, stores it, and serves strings by page + language over a small read-only
HTTP API.

Single Go package at the repo root (`package main`, flat layout). Go 1.23, stdlib `net/http`
with `ServeMux` method patterns, `excelize` for XLSX, `kin-openapi` for schema generation,
distroless static image.

## Commands

```sh
go test ./...
go run . --schema          # regenerates openapi.json
go build .
docker compose up --build
helm lint helm/
./helm/update-snapshots.sh # needs the fugit submodule initialized
```

`helm/snapshots/*.yaml` are committed `helm template` renders checked by CI — regenerate them
whenever chart or values files change, or CI fails.

## Architecture constraints

These are decisions, not defaults. Check before adding anything.

**No managed cloud services.** The chart deploys to two clusters with different owners —
IFRC's AKS (`helm/values/go-deploy.yaml`: workload identity, Key Vault CSI) and Toggle's own
cluster (alpha, `longhorn` storage class). Anything tied to a specific cloud provider breaks
portability across them. Kubernetes' own API server is acceptable infrastructure; managed
provider offerings are not.

**Dependency-light.** Justify any new dependency with measured cost, not assertion. The
Kubernetes Lease client is hand-rolled (~90 lines, stdlib only) because `client-go` measured
+20.4 MB and +77 modules against a 15 MB / 71 module baseline — the clientset links every API
group whether used or not. Build a scratch binary and compare before reaching for a library.

**Health endpoints are split by consumer, and must stay split.** Conflating them caused a
production incident where the backend was locked, every probe reported healthy, and `/strings`
was failing.

| endpoint | consumer | may fail on a dependency? |
|---|---|---|
| `/healthz` | kubelet liveness | never |
| `/readyz` | kubelet readiness | only on this pod's own data |
| `/status` | humans | no, always 200 |
| `/cluster` | humans | no, always 200 |
| `/monitor` | UptimeRobot | yes — that is its job |

`/monitor` is the only endpoint allowed to go red on a dependency. Never route staleness,
leader-election, or upstream failures into `/readyz`: every pod would go unready at once,
turning "data is stale" into "service is down".

**A readiness check must exercise the same code path the API uses**, not a parallel one. The
original incident was `/status` querying the `meta` table while `/strings` queried `strings`.

## Known scaling limits

1. **Deploy-time availability.** `replicaCount: 1` + `strategy: Recreate` + RWO PVC makes every
   deploy and node drain a full outage. `helm/README.md` records the `azurefile-csi` incident
   that led to the current single-pod setup.
2. **Read throughput.** Not a problem at one application; it becomes one when several
   applications' translation caches share the service. Multi-tenancy is anticipated, so prefer
   keying new internal state by application id over assuming a single app.

Unmeasured and worth knowing before tuning pod resources: the real production XLSX size, its
language count, and peak memory during import. `ParseXLSX` uses `excelize.GetRows`
(`xlsx.go:44`), which materializes the whole sheet at once.

## Gotchas in the current code

- `db.go:44` — `SetMaxOpenConns(1)` serializes every request behind every other request and
  behind the entire import transaction.
- `handlers.go:73-83` — `/healthz` and `/readyz` are byte-identical hardcoded 200s.
- `handlers.go:162` and `:171` — `/strings` reads the ETag and the body in two separate DB
  round trips, which can straddle an import and return a mismatched pair.
- `sync_pull.go:150-186` — a failed initial pull is retried a full `PULL_INTERVAL` later
  (10 min in production). Harmless only because `/readyz` never fails today.
- `handlers.go:51-52` — recovery wraps *outside* logging, so a panicking request never emits
  its request log line. `recovery.go` also captures no stack trace.
- CI runs `go test` with default CGO, so tests exercise `mattn/go-sqlite3` while production
  builds with `CGO_ENABLED=0` and runs `modernc` (`Dockerfile:6`).
- `helm/values.yaml` `sqlite.pvc.accessModes` is dead — `helm/templates/pvc.yaml` hardcodes it.
- `docker-compose.yml` sets `SQLITE_TMP_DIR`, which nothing reads.
