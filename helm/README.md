## Strategy + Azure storage

### Why these errors happen (short)
- `FailedAttachVolume` / `FailedVolumeAttach`: usually occurs when the PVC is backed by ReadWriteOnce (RWO) storage and Kubernetes attempts to start a new Pod before the old Pod has fully released/detached the volume.Because the same volume can’t be attached to two Pods/nodes at once, the attach step fails and the rollout can get stuck.
- `database is locked` (SQLite): after switching the PVC `storageClass` to `azurefile-csi`, the app started hitting SQLite lock errors.

We observed these issues during rollouts on Azure when multiple Pods briefly overlapped.

### Current approach (what we use)
We use `strategy.type: Recreate` so upgrades run only one Pod at a time:
- switched back to the default storage class
- `Recreate` avoids parallel Pod overlap, which prevents volume attach conflicts and reduces the chance of SQLite locking problems
