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
| `poddisruptionbudget.yaml` | `maxUnavailable: 1`, so a drain always leaves a warm peer to hydrate from, at any replica count |
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

`/monitor` answers 503 when a fleet alarm fires: no primary, no successful
pull for `ALARM_SNAPSHOT_AGE`, replicas holding different snapshots, an
unhealthy pod, or repeated pull failures. None of that makes a pod unready, because routing
staleness into readiness would turn a data problem into a total outage.

Nothing falls back automatically when no pod can pull, so this alert is the
thing that gets it noticed. Point the uptime check at `/monitor`.

## Changing configuration

`envFrom` is read once when a container starts, so a changed `env:` value
reaches the ConfigMap but not a running pod. The pod template carries a
`checksum/config` annotation over the rendered ConfigMap, so `helm upgrade`
rolls the pods whenever configuration actually changes. Do not remove it on
the assumption that the stakater reloader annotation covers this -- that
depends on a controller being installed in the target cluster, and the chart
should be correct without one.

### Configuration this chart does not create

`extraConfigMapName` and `extraSecretsName` name a ConfigMap and a Secret that
already exist in the namespace. Both are added to `envFrom` after the chart's
own, so a key set in either wins.

`checksum/config` covers only the ConfigMap this chart renders, so a change to
an external object does not roll the pods by itself. Where the stakater
reloader controller is installed, the Deployment's annotation covers them;
where it is not, restart the pods.

## Ingress annotations

`ingress.annotations` is rendered through `tpl`, so a string value may
reference release data (`{{ .Release.Namespace }}`); a non-string value is
rendered as JSON, and a null value drops its key. Either way the value reaches
Kubernetes as the string it requires.

CI writes deployment provenance into this block before packaging the chart:
`web-app-serve.togglecorp.com/*` keys carrying the commit, ref, PR, image,
actor and run URL, per
[`annotations-v1.json`](https://raw.githubusercontent.com/toggle-corp/web-app-serve-action/main/schema/annotations-v1.json).
The Togglecorp dashboard reads them off the live Ingress to show what is
deployed. The write is a `yq` path assignment into `values.yaml`, so the
`annotations:` key has to stay present there even when empty.

Because the values pass through `tpl`, a literal `{{` in a commit subject
would fail the render at deploy time rather than in CI. The CI step escapes it
before writing.

## Snapshots

`helm/snapshots/*.yaml` are committed renders checked by CI. Regenerate them
after any chart or values change:

```sh
git submodule update --init fugit
./helm/update-snapshots.sh
```
