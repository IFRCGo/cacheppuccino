# cacheppuccino

Translation caching service for IFRC GO. Periodically downloads an XLSX export from a
translation service, holds it in memory, and serves strings by page + language over a small
read-only HTTP API. Three stateless replicas; one of them, elected through a Kubernetes
Lease, does the pulling and the others hydrate from it.

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
| `/healthz` | kubelet liveness | never — only on its own sync loop having stopped ticking |
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

1. **Read throughput.** Not a problem at one application; it becomes one when several
   applications' translation caches share the service. Multi-tenancy is anticipated, so prefer
   keying new internal state by application id over assuming a single app.
2. **Fleet-wide data loss.** The snapshot cache is an `emptyDir`, so it survives container
   restarts within a pod and nothing else. Losing every replica at once (a node-pool upgrade,
   an involuntary eviction of all three) leaves no copy anywhere, and the fleet cannot serve
   until upstream comes back.

Unmeasured and worth knowing before tuning pod resources: the real production XLSX size, its
language count, and peak memory during import. `ParseXLSX` uses `excelize.GetRows`
(`xlsx.go:47`), which materializes the whole sheet at once, and each `Snapshot` also retains
the raw XLSX it was built from so peers can hydrate from it.

## Gotchas in the current code

- Lease timestamps are `metav1.MicroTime`, which parses with exactly six fractional digits
  (`leaseTimeLayout` in `lease.go`). `time.RFC3339Nano` output is rejected by the API server,
  and `lease_test.go`'s fake decodes into the client's own `*string` fields, so it cannot
  catch a format regression.
- `/monitor` is the only endpoint allowed to go red. Alarm thresholds come from
  `ALARM_*`; `ALARM_SNAPSHOT_AGE` measures the last *successful pull*, not the snapshot's
  import time, because an export that simply does not change leaves `ImportedAt` pinned.
- `/cluster` is served on the public listener and carries pod names and pod IPs. The
  internal listener (`:8081`, `internal_handlers.go`) is unauthenticated and absent from the
  Service and the ingress; there is no NetworkPolicy, so any workload in the cluster can read
  a snapshot from it.
- `Snapshot` maps are shared, not copied: `Snapshot.Get` returns the live inner maps and
  `emptyStrings` is package-level. Nothing may write through a value reachable from a
  snapshot.
- `pullApp` installs with `Holder.Store`; the peer path uses `Holder.StoreIfNewer`. Versions
  are `ImportedAt.UnixMilli()` stamped by whichever pod pulled, so clock skew between pods
  shows up as adoption churn.
- `PeerClient.Discover` excludes this pod by comparing resolved addresses to `POD_IP`. With
  `POD_IP` unset, or on a dual-stack cluster, a pod surveys itself and counts twice.
- `AppState.consecutiveFailures` is cleared only by `recordSuccess`, which only the primary
  reaches. A demoted pod therefore carries its streak for life, so `worstPullFailure` counts
  only pods reporting `Primary`.
- `helm/tests/alpha.yaml` pins `requests.memory == limits.memory == 512Mi` against a chart
  default of 256Mi/1Gi. That is Guaranteed QoS with no burst headroom, and alpha's
  `PULL_INTERVAL: 1m` means all three replicas parse the same export within seconds of each
  other while the old snapshot is still live.
- CI runs `go test` without `-race`, and `go mod tidy` + `git diff --exit-code` is a required
  step.
