## Topology

Three stateless replicas behind a `RollingUpdate` with `maxUnavailable: 0`.
Nothing is stored on a PersistentVolume, so there is no storage class, no
`ReadWriteOnce` attach step, and none of the failure modes below.

### What used to go wrong

The chart previously ran a single pod with `strategy: Recreate` and a
`ReadWriteOnce` PVC holding a SQLite file:

- `FailedAttachVolume` / `FailedVolumeAttach`: the same `ReadWriteOnce` volume
  cannot be attached to two pods at once, so a new pod could not start until
  the old one had fully detached, and rollouts stalled.
- `database is locked`: after the PVC `storageClass` was switched to
  `azurefile-csi`, SQLite hit lock errors. SMB does not carry POSIX advisory
  locking or WAL shared memory reliably.

`Recreate` worked around both by never letting two pods overlap, at the cost
of making every deploy a full outage.

### How it works now

Translations are held in memory and served with no I/O. A pod that starts
cold gets its data, in order of preference, from:

1. its `emptyDir` cache, which survives container restarts within the pod;
2. a sibling pod, over the headless Service;
3. upstream.

Only the primary talks to upstream. It is elected through a
`coordination.k8s.io/v1` Lease, and if it stops renewing, another pod takes
over after `LEASE_DURATION`.

Because a pod reports ready only once it holds servable translations, and
`maxUnavailable: 0` keeps the old pod serving until the new one is ready, a
rollout that cannot reach upstream stalls with the old pod still serving
rather than dropping traffic.

## Resources this chart creates

| Template | Purpose |
|---|---|
| `deployment.yaml` | The pods, with both listeners and all three probes |
| `service.yaml` | Public `ClusterIP` on 8080 |
| `service-headless.yaml` | Peer discovery; publishes not-ready addresses so a broken pod stays visible on `/cluster` |
| `rbac.yaml` | `Role`/`RoleBinding` for `get`/`create`/`update` on one Lease |
| `poddisruptionbudget.yaml` | `minAvailable: 2`, so a drain always leaves a warm peer to hydrate from |
| `hpa.yaml` | Disabled; replicas buy availability today, not throughput |
| `configmap.yaml`, `secret.yaml`, `secret-provider-class.yaml` | Configuration and Key Vault secrets |
| `ingress.yaml` | Traefik ingress for the public port only |

The internal port is deliberately absent from `service.yaml` and the
ingress: the ingress routes `/` as a prefix, so anything on the public port
is reachable from the internet.

## Health endpoints

Three consumers, three endpoints, and they must not be conflated -- that is
what produced an incident where every probe was green while the API failed.

| Endpoint | Consumer | Fails on a dependency? |
|---|---|---|
| `/healthz` | kubelet liveness | Never, except a sync loop that has stopped ticking |
| `/readyz` | kubelet readiness | Only on this pod's own data |
| `/monitor` | External uptime check | Yes -- it is the only one that may |

`/monitor` answers 503 when a fleet alarm fires: no primary, a stale
snapshot, replicas holding different snapshots, an unhealthy pod, or
repeated pull failures. None of that makes a pod unready, because routing
staleness into readiness would turn a data problem into a total outage.

Nothing falls back automatically when no pod can pull, so this alert is the
thing that gets it noticed. Point the uptime check at `/monitor`.

## Snapshots

`helm/snapshots/*.yaml` are committed renders checked by CI. Regenerate them
after any chart or values change:

```sh
git submodule update --init fugit
./helm/update-snapshots.sh
```
