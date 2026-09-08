# Code Review: File-Copy CSI Volume Migration (branch `csi-migration-with-pvc-transfer`)

Reviewer: automated design/code review
Date: 2026-09-02
Scope: uncommitted changes on this branch implementing cross-vCenter CSI
(filesystem) volume migration via rsync file copy, reviewed for **correctness**
and **data safety**.

## Files reviewed

New:

- `api/v1alpha1/volumecopy_types.go` — `VolumeCopy` CRD + phase/rebind enums.
- `internal/storagecopy/{mover,engine,transfer,verify,topology,inventory}.go`
  — the `VolumeCopy` reconciler and data mover.
- `internal/controller/storage_migration.go` — planning + aggregation on the
  singleton `VmwareCloudFoundationMigration`.
- `internal/controller/storage_cleanup.go` — gated source-volume deletion.
- `config/crd/bases/migration.openshift.io_volumecopies.yaml`.

Modified:

- `api/v1alpha1/vmwarecloudfoundationmigration_types.go` — `StorageMigrationSpec`.
- `internal/controller/preflight.go` — relaxed CSI gates for copy.
- `internal/controller/vmwarecloudfoundationmigration_controller.go` — wires the
  `StorageMigrated` and `SourceCleaned` steps; RBAC.
- `cmd/main.go`, `config/rbac/role.yaml`, `go.mod`/`go.sum`, vendored deps.

## Verdict

**The implementation faithfully realizes the hardened design and its
data-loss-critical invariants are present and correctly ordered.** Source data
is structurally protected throughout the forward path. I found **no confirmed
data-loss defect** in the destructive cutover/cleanup paths.

However, there is **one HIGH-severity correctness bug that likely makes the
feature non-functional as written** (CNSVolumeInfo namespace, H1) and several
availability / operability gaps that should be fixed before this is usable in
anger. All of the failure modes I found are **fail-closed** (they stall the
migration) rather than fail-open (they do not silently destroy data), which is
the right bias.

---

## Confirmed data-safety strengths

These map directly to the design-review findings and are correctly implemented:

- **Source is never writable during copy.** The rsync source pod mounts the
  source PVC read-only three ways: PVC volume source `ReadOnly:true`, the
  volumeMount `ReadOnly:true`, and the container is the only mounter
  (`engine.go:138,143`). rsync `--delete` therefore can never act on the source.
- **D1 — Retain verified persisted before any PVC delete.** `rebindOriginalName`
  patches both PVs to `Retain`, then re-GETs and asserts persistence via
  `ConfirmRetainPolicy` for both before advancing to `PVsRetained`
  (`mover.go:784-804`, `verify.go:75`). The patched-consumer cleanup path does
  the same (`storage_cleanup.go:121-138`).
- **D2 — claimRef reservation (not clearing).** Target PV is pinned to the
  original PVC name via `ReservePVForClaim` (namespace+name, empty UID) and the
  recreated PVC pins back with `spec.volumeName` — a correct two-way static
  rebind (`verify.go:87`, `mover.go:812,841`). No `Available` window for a rogue
  PVC to steal the volume.
- **D3 — destructive-sync guard.** Before the authoritative `--delete` pass,
  `VerifyDestructiveSyncGuard` asserts `sourceHandle != targetHandle` and that
  the target handle is owned by the target vCenter (`verify.go:62`,
  `mover.go:272`). `prepareTarget` independently rejects a target PV that reuses
  the source handle (`mover.go:239`).
- **D4/P1 — durable rebind sub-state before destructive steps.** Source PV
  identity is persisted at `initialize` (`mover.go:193`); each `RebindPhase` is
  persisted before the next state acts, so `TargetReserved` (target PV pinned to
  the original name) is durable *before* the source PVC is deleted
  (`mover.go:769-857`, enum in `volumecopy_types.go:45-52`).
- **D6 — receiver unmount gate.** `SourceBound` waits for the target PV's
  `VolumeAttachment` to disappear before flipping policies/deleting
  (`mover.go:777-783,915`).
- **I3 — never simultaneously gone.** At every rebind sub-state at least one
  retained, status-recorded PV holds each copy; the source PV stays `Retain`
  from `SourceBound` onward.
- **F3 — capacity from actual, not requested.** Target PVC is sized
  `max(request, source PV status.Capacity)` (`mover.go:555-566`).
- **Source cleanup is gated and idempotent.** Deletion requires every
  `VolumeCopy` `Succeeded`, a rollback-retention window measured from
  authoritative completion (or explicit ack), re-verifies retained-PV identity
  (UID + handle) and `Retain`, refuses if the PV is still `Bound`, and
  CNS-deletes only after a query, verifying removal
  (`storage_cleanup.go:35-95,140-197`).
- **Preflight reconciled (I1).** When storage migration is enabled the old
  "no vSphere CSI PVs / CSI must be Removed" gates are replaced by
  `checkFileCopyPreflight` + a "CSI must be **Managed**" gate
  (`preflight.go:110-121,193-256`).

---

## Findings

### HIGH

#### H1 — `CNSVolumeInfo` is queried in the workload namespace; it almost certainly lives in `vmware-system-csi` → ownership can never be verified (likely breaks the whole feature)

`ResolveVolumeOwner` lists `CNSVolumeInfo` with
`client.InNamespace(namespace)` where `namespace` is the **workload PVC
namespace** (`verify.go:17,30,33`; callers pass `copy.Namespace` at
`mover.go:170,242,272`). The vSphere CSI driver publishes
`cnsvolumeinfoes.cns.vmware.com` in the **`vmware-system-csi`** namespace, not
in each workload namespace.

Failure scenario: with CNSVolumeInfo in `vmware-system-csi`, the list returns
zero items for a workload-namespace query → `"volume handle … has no
CNSVolumeInfo ownership record"`. `initialize` then fails, and even if it
passed, `prepareTarget` would loop forever "waiting for positive target CNS
ownership verification" (`mover.go:242-245`). The migration can never leave the
planning/prepare stage. This is fail-closed (safe) but non-functional.

Fix: query the namespace where CNSVolumeInfo actually resides (make it a
constant / configurable, defaulting to `vmware-system-csi`), or list
cluster-wide if the CR is cluster-scoped in the target CSI version. **Verify the
CR's scope and namespace against the actual driver build** before merge; add an
integration test that seeds CNSVolumeInfo in the real namespace. The current
unit tests presumably seed it in the workload namespace and therefore mask this.

#### H2 — cutover pins *all* replicas to a single target node and never reverts it

`constrainToTarget` sets `NodeSelector[kubernetes.io/hostname] = <one node>` on
the **whole** pod template for both `RebindOriginalName` (via
`scaleRecordedWorkloads(-1)`, `mover.go:717-719,735-736`) and `PatchConsumer`
(`mover.go:873,885`). This is applied to the Deployment/StatefulSet spec and is
**never removed**.

Failure scenario: a 3-replica Deployment is collapsed onto one worker after
cutover — losing HA, possibly exceeding node capacity (pods stuck `Pending`), and
permanently wedded to a node that may later be drained/removed, after which the
workload cannot schedule at all. For RWX this is gratuitous; for RWO the volume's
own topology (PV `nodeAffinity` + zone-constrained StorageClass, already
validated in `topology.go:93`) is sufficient — a *zone* constraint, not a single
hostname, is what's needed.

Fix: constrain to the target failure domain (region/zone labels) rather than a
single hostname, and/or record and restore the original `NodeSelector` so the
pin is temporary.

#### H3 — Automatic mode stalls on any workload controller that isn't Deployment/StatefulSet, with no preflight detection

`discoverWorkloadState` supports only Deployment (via ReplicaSet) and
StatefulSet; anything else — DaemonSet, Job/CronJob, bare pods, or third-party
controllers (e.g. Argo Rollouts, OLM operands) — returns an error
(`mover.go:657,681`). Planning (`planFileCopies`) does not detect this and still
emits a `Copy` plan in `Automatic` mode.

Failure scenario: a namespace with an RWX PVC mounted by a DaemonSet plans fine,
then the `VolumeCopy` reconcile errors indefinitely at `Quiescing`. Additionally,
a PVC whose consumers have no controller gets `workloadGroup = "pvc-<uid>"`
(`storage_migration.go:214`); in `Automatic` `quiesce` the `workload-` prefix
check is false (`mover.go:372`), so no state is recorded and, with a live writer,
it loops forever (and `reconcileTransfer(final)` would reject the writer anyway,
`mover.go:279`). Fail-closed, but a silent runtime stall.

Fix: in `checkFileCopyPreflight`, classify each source PVC's consumers and
reject (or force `Manual`) any volume whose controller is unsupported by
automatic quiesce, so the user learns at preflight rather than mid-migration.

### MEDIUM

#### M1 — RWX writer can reappear between the final sync and source-PVC deletion (residual D5)

`reconcileTransfer(final)` re-checks writers immediately before and after the
authoritative pass (`mover.go:275-281,308-314`), but nothing re-checks writers
during `CuttingOver`. Correctness for RWX therefore depends entirely on the
quiesced workloads *staying* at zero replicas until the source PVC is deleted.

Failure scenario: for a `PatchConsumer` cutover or any RWX volume, if an
un-suppressable third-party controller rescales a writer after the final sync,
it writes to the still-present source PVC; those writes are never copied and are
lost when the source is deleted. RWO is protected by the VolumeAttachment gate;
RWX is not.

Fix: document RWX-with-un-suppressable-controllers as `Manual`-only (design
already implies this), and consider a last-moment writer re-check at the start of
`cutOver` for RWX.

#### M2 — deleting the `VolumeCopy` mid-rebind can strand the workload

The finalizer only calls `cleanupTransferResources` and then removes the
finalizer (`mover.go:67-76`). If a `VolumeCopy` is deleted while `RebindPhase`
is `SourcePVCDeleted` (original PVC gone, replacement not yet created), the CR
disappears and nothing recreates the original PVC.

Failure scenario: operator/user deletes the `VolumeCopy` during cutover → the
workload's PVC is missing and it stays down. Data is **not** lost (the target PV
is retained and reserved), but recovery is manual. 

Fix: block deletion (refuse to drop the finalizer) while `RebindPhase` is in a
destructive, non-terminal state, or make the finalizer complete/repair the
rebind before releasing.

#### M3 — ownership verification has no fallback to a direct CNS query

Ownership (and the `--delete` guard) rely solely on `CNSVolumeInfo`
(`verify.go`). The Broadcom report documents 30–60 s target-side CNS
registration lag, and CNSVolumeInfo may be absent/stale. The cleanup path
already talks to CNS directly (`storage_cleanup.go:140`), but verification does
not.

Impact: absence/lag → indefinite stall (safe). Consider a direct
`CnsQueryVolume` fallback with a bounded poll so a lagging syncer doesn't wedge
the migration, while still treating *absence* as "await/verify," never "assume."

#### M4 — source PV `Retain` not re-confirmed immediately before the source PVC delete

`Retain` on the source PV is confirmed in `PVsRetained` (`mover.go:798`) but the
source PVC is deleted a state later in `TargetReserved` (`mover.go:824`) using a
PV object re-fetched at the top of the function without re-asserting the policy.

Failure scenario (narrow): if an external actor flips the source PV back to
`Delete` between the two states, deleting the source PVC reclaims and destroys
the source volume. Low likelihood, but it's the one place a cross-reconcile
TOCTOU touches an irreversible delete.

Fix: call `ConfirmRetainPolicy(sourcePV)` again immediately before
`r.Delete(pvc)` in `TargetReserved` — cheap insurance on the last destructive
step.

#### M5 — `bandwidthLimit` accepts only a bare KiB integer, but the API documents `"100Mi"`

`BuildRsyncOptions` does `strings.TrimSuffix(value, "Ki")` then `Atoi`
(`transfer.go:36-39`), so only `"<int>"` or `"<int>Ki"` parse. The field doc
says *"an optional rsync bandwidth limit such as `\"100Mi\"`"*
(`vmwarecloudfoundationmigration_types.go:117`).

Failure scenario: a user follows the documented example `"100Mi"` →
`Atoi("100Mi")` fails → `EnsureAttempt` errors at precopy/final build time
(runtime, not preflight) → stall. The mismatch is also not caught by
`checkFileCopyPreflight`.

Fix: parse resource.Quantity-style suffixes (Ki/Mi/Gi) and/or validate the
format in preflight; align the doc and CRD with what the parser accepts.

### LOW / observations

- **L1 — `go_vulncheck`**: the only findings are Go **standard-library**
  advisories against the pinned toolchain (`go1.26.1`); none are in the
  migration code itself. Bumping the Go toolchain clears them. (Full list
  captured during review; all stdlib: `crypto/x509`, `net/http`, `html/template`,
  `crypto/tls`, etc.)
- **L2 — `copy` shadows the builtin** throughout `mover.go`/`engine.go`. Harmless
  but a readability smell.
- **L3 — precopy `TransferCompletedAt`** is written on both precopy and final
  attempts (`mover.go:299`); the final attempt (separate `Attempt`, separate pod)
  overwrites it, so the retention clock correctly anchors on the authoritative
  copy. Worth a comment so a future edit doesn't invert the order.
- **L4 — Positive:** `ClassifyVolume` correctly forces block volumes and
  `Copy`-strategy raw block to `CNSRelay`/`Manual` and keeps rsync to filesystem
  mode only (`inventory.go:40-52`) — the P2 exclusion holds. RWOP precopy is
  disabled (`mover.go:590`).
- **L5 — Positive:** transfer path is private (ClusterIP + stunnel TLS +
  NetworkPolicy restricting ingress to the sender, `engine.go:62-97,190-204`).

---

## Recommended must-fix before merge

1. **H1** — fix the CNSVolumeInfo namespace/scope; add a test that seeds it where
   the driver actually creates it. Without this the feature does not run.
2. **H2** — stop pinning whole workloads to a single hostname; constrain to the
   target zone and/or restore the original `NodeSelector`.
3. **H3** — detect unsupported/uncontrolled consumers in preflight and force
   `Manual`.
4. **M4** — re-confirm source-PV `Retain` immediately before the source PVC
   delete.
5. **M5** — reconcile the `bandwidthLimit` format between parser, CRD doc, and
   preflight.

Items **M1–M3** and the **L** notes are follow-ups; none is a data-loss defect.
