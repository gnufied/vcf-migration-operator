# Topology-Aware File-Copy Volume Migration Across vCenters

Status: Draft / Proposal (Phase 2 — Storage)

Related design: `docs/plans/csi-volume-migration.md`

## 1. Decision Summary

Add a topology-aware `FileCopyMover` to `vcf-migration-operator` for
filesystem-mode PVCs. The mover provisions a new volume on the target vCenter,
copies data with the `github.com/migtools/pvc-transfer` rsync implementation,
quiesces writers for a final authoritative sync, and cuts the workload over to
the target volume.

This is an in-place OpenShift cluster migration. Kubernetes API objects and
etcd are not moved to a second cluster, so Crane's export/transform/apply
pipeline is not part of this design. Only the reusable PVC data-transfer
library is used. Workload changes are narrow, recorded patches or a PVC
same-name rebind; the operator does not export and recreate a namespace.

The initial strategy matrix is:

| Volume | Default action |
|---|---|
| vSphere CSI, `volumeMode: Filesystem`, RWX/ROX | `FileCopyMover` |
| vSphere CSI, `volumeMode: Filesystem`, RWO | `FileCopyMover`; online precopy only when a reader can share the workload's node |
| vSphere CSI, `volumeMode: Filesystem`, RWOP | `FileCopyMover`, offline copy only |
| vSphere CSI, `volumeMode: Block` | CNS relay or explicit manual handling; rsync is not applicable |
| External NFS, CephFS, object storage, or other vCenter-independent storage | `NoMove` after target-node reachability validation |

The staged CNS relay remains an optional identity-preserving backend for raw
block volumes and workloads where recreating/rebinding a PVC is unacceptable.
It is not a prerequisite for file-copy-only migrations, and a shared staging
datastore is not required when all selected volumes use `FileCopyMover`.

## 2. Goals and Non-Goals

### Goals

- Migrate vSphere CSI filesystem volumes between vCenters without relying on
  cross-vCenter CNS relocation.
- Work when existing source PVs have no `spec.nodeAffinity`.
- Guarantee that source readers run on source-vCenter nodes and destination
  receivers run on the selected target-vCenter failure domain.
- Support an optional online seed copy followed by a write-quiesced final sync.
- Resume safely after operator, pod, node, or API-server restarts.
- Keep the source volume intact until target validation and an explicit
  rollback-retention period complete.
- Provide a specialized OpenShift integrated image-registry cutover.

### Non-goals for v1

- Crash-consistent live migration without a final write quiesce.
- Raw block copying with rsync.
- Preserving CSI snapshots or the source volume handle.
- Application-specific database backup protocols. Applications that cannot be
  made filesystem-consistent require `Manual` mode or application-native
  replication/backup.
- Cross-cluster Kubernetes resource migration.

## 3. Required Invariants

The controller must enforce all of the following rather than relying on the
scheduler to make a favorable choice:

1. `MultiSiteConfigured=True` and the vSphere CSI components are healthy before
   any target PVC is created.
2. At least one Ready, schedulable target worker exists in every selected target
   failure domain before copy starts. Target worker creation is therefore moved
   ahead of `StorageMigrated`, as already proposed by the CNS design.
3. A source reader and destination receiver never need to mount both PVCs. Each
   pod mounts exactly one side and transfers data over the cluster network.
4. Source placement is explicit. Missing source-PV node affinity is not used as
   evidence that a volume is globally accessible.
5. Destination provisioning and destination-pod placement resolve to the same
   target region/zone and vCenter.
6. The destination PV is verified as target-owned before any final copy or
   cutover. Ownership is *positively verified* (poll-and-block with timeout),
   never assumed from the absence of contrary evidence. A `--delete` sync or a
   cutover must not proceed on an unverified volume handle.
7. Online precopy is only a performance optimization. A copy is authoritative
   only after all writers are quiesced and the final rsync succeeds.
8. A resumed workload is constrained to target nodes while both vCenters are
   configured, even if the new PV lacks useful node affinity.
9. Source data is never deleted as part of forward migration.
10. Before deleting any PVC whose PV must survive, the controller re-reads the
    PV and confirms `persistentVolumeReclaimPolicy == Retain` has been
    persisted. A PVC delete against a not-yet-`Retain` PV is forbidden, because
    reclaim of a `Delete`-policy PV destroys the backing CNS/FCD volume.
11. At every intermediate state of a cutover there exists at least one retained,
    identifiable PV holding the data, and its identity is recorded in
    `VolumeCopy.status`. The data is never simultaneously unbound and
    unrecorded (mirrors the CNS relay's "never simultaneously unregistered and
    absent" property).
12. A destructive rsync (`--delete`) never runs unless the receiver's mounted
    PV `volumeHandle` differs from the source PV `volumeHandle` and resolves to
    the target vCenter.

## 4. Pipeline Integration

Add `StorageMigrated` between `MultiSiteConfigured` and the remainder of
`WorkloadMigrated`:

```text
InfrastructurePrepared
DestinationInitialized
MultiSiteConfigured
TargetCapacityReady        target worker MachineSets exist and nodes are Ready
StorageMigrated
  PlanVolumes
  PrepareTargets
  PreCopy                  optional and non-authoritative
  QuiesceAndFinalCopy      per workload group
  CutOverAndValidate
WorkloadMigrated           roll remaining compute; drain/scale source workers
SourceCleaned
Ready
```

`TargetCapacityReady` may be an internal sub-state of `StorageMigrated` rather
than a public condition, but its completion must be derived from MachineSet and
Node state, not from a status message string.

## 5. Inventory and Classification

For every bound PVC, record:

- namespace/name/UID, PV name/UID, reclaim policy, capacity and volume mode;
- access modes, StorageClass, CSI driver and volume handle;
- consuming pods and their owning controllers;
- all nodes currently mounting or consuming it;
- source vCenter ownership from `CNSVolumeInfo` and/or a CNS query;
- source StorageClass parameters and provisioner;
- whether the workload has a built-in cutover adapter;
- requested destination StorageClass and failure domain.

Do not classify a volume from access mode alone. RWX describes Kubernetes
access semantics; it does not prove that the backing store is vSAN File
Services or that it must move. External storage is classified `NoMove` when it
remains reachable and supported from target workers.

Group PVCs by workload. All PVCs required by one pod template are quiesced and
cut over together so a pod cannot restart with a mixture of source and target
data.

## 6. Topology Resolution

### 6.1 Source reader placement

Source PVs may predate topology-aware provisioning and have no node affinity.
The controller resolves source placement independently:

1. Identify the vCenter owning the CSI volume using `CNSVolumeInfo`; confirm it
   equals the migration source vCenter. Fall back to source/target CNS queries
   only when the CR is absent, and fail on ambiguous or dual ownership.
2. Map Ready Nodes to Machines, and Machines to vCenters using the Machine
   provider spec `workspace.server`. Cache a `node -> vCenter` map for the
   reconciliation pass.
3. If a running workload pod already mounts the PVC:
   - for RWO, use that exact `spec.nodeName`, because another RWO pod may mount
     the volume only on the same node;
   - for RWX, prefer the existing node but allow another verified source node
     when necessary.
4. For an idle RWX volume, select a Ready, schedulable worker whose Machine is
   owned by the source vCenter and whose region/zone labels are compatible with
   any topology metadata available from CNS.
5. For an idle RWO/RWOP volume, run a read-only mount probe on a candidate
   source node before selecting it. RWOP is only probed after all consumers are
   stopped.

The source transfer pod receives either an exact `NodeName` (active RWO) or a
required node affinity/selector generated from verified source nodes. A generic
`topology.kubernetes.io/zone=<value>` selector is insufficient when the same
tag value exists in both vCenters; the final candidate set must also be checked
against Machine `workspace.server`.

The controller records the chosen node name, Node UID, Machine name, vCenter,
region and zone. Before creating each copy attempt it revalidates that the node
still exists, is Ready, and still maps to the source vCenter.

### 6.2 Destination provisioning and receiver placement

The migration API maps each source StorageClass to an explicit target
StorageClass and target failure domain. The target StorageClass must use the
same CSI capability required by the PVC and must not be able to provision into
the source vCenter accidentally.

Preferred configuration:

- `provisioner: csi.vsphere.vmware.com`;
- `volumeBindingMode: WaitForFirstConsumer`;
- `allowedTopologies` limited to the target region/zone;
- a target-vCenter storage policy capable of the requested access and volume
  mode.

The receiver pod is created with required affinity for exactly the same target
region/zone. With `WaitForFirstConsumer`, this pod drives target provisioning.
The pod may also carry target-specific tolerations configured in the migration
spec.

`Immediate` binding is allowed only if `allowedTopologies` uniquely selects the
target failure domain. After binding, the controller must verify target
ownership before starting the receiver. An unconstrained `Immediate`
StorageClass is rejected.

After the PVC binds, verify all of the following:

- the receiver was scheduled on a Node whose Machine `workspace.server` is the
  selected target vCenter;
- the receiver Node has the expected region/zone labels;
- the new PV's node affinity, when present, permits the selected target Node
  and excludes all verified source Nodes;
- `CNSVolumeInfo` maps the new volume handle to the target vCenter;
- a target-side CNS query finds the volume and its policy/capacity matches the
  request.

If the new PV has no node affinity, migration may continue only when the
workload cutover adapter can add and retain an equivalent target-node placement
constraint through source cleanup. Otherwise fail before copying data.

### 6.3 Scheduling race prevention

- Do not create a destination PVC against an unconstrained StorageClass and
  patch topology later; it may already be provisioned on the wrong vCenter.
- Do not set only `spec.nodeName` on a target receiver without validating its
  Machine and labels. `nodeName` bypasses scheduler filtering.
- Do not infer source ownership from a missing PV node affinity.
- Do not remove temporary target workload placement until all source workers
  are drained or the new PV itself provides durable target-only affinity.

## 7. Data-Mover Architecture

Add a pinned dependency on `github.com/migtools/pvc-transfer` and use its rsync
server/client primitives directly. Do not shell out to the Crane CLI.

New packages:

```text
internal/storagecopy/
  mover.go              FileCopyMover state machine
  inventory.go          PVC/PV/consumer classification
  topology.go           node/Machine/vCenter and SC topology resolution
  target.go             destination PVC preparation and ownership checks
  transfer.go           pvc-transfer adapter
  cutover.go            PVC rebind and consumer patch strategies
  verify.go             rsync and application-level validation
internal/openshift/
  registry.go           integrated image-registry adapter
  workload_quiesce.go   controller discovery, quiesce and resume
```

### 7.1 Required pvc-transfer extensions

The library already accepts `PodOptions.NodeName`, `NodeSelector`, security
contexts, resources and custom rsync command options. The operator additionally
needs:

1. A read-only source PVC mount option. Both the `VolumeMount.ReadOnly` and
   `PersistentVolumeClaimVolumeSource.ReadOnly` fields must be set on reader
   pods.
2. Required pod affinity, tolerations and optionally a scheduler name. A plain
   `NodeSelector` is not expressive enough for a verified set of source Nodes
   spanning zones.
3. A private in-cluster endpoint implementation backed by a ClusterIP Service.
   Route/Ingress creation is unnecessary because both pods are in one cluster.
4. Stable attempt names supplied by the caller so reconciliation adopts
   existing resources instead of creating duplicates.
5. Transfer status that exposes exit code, start/completion time, bytes/files
   copied and the pod identity without requiring log parsing for correctness.

Contribute generally useful changes upstream where practical. Keep the
ClusterIP endpoint and operator state machine local if they are specific to
this same-cluster workflow.

### 7.2 Transport and security

- Run source reader and destination receiver in the PVC's namespace; pods
  cannot mount claims from another namespace.
- Use a ClusterIP Service plus the library's stunnel transport so data remains
  encrypted across the source/target site network.
- Create an ephemeral per-attempt credentials Secret and restrict it to the two
  transfer pods.
- Apply a NetworkPolicy permitting only reader-to-receiver traffic.
- Pin the transfer image by digest and run with the restricted SCC by default.
- Do not create public Routes, Ingresses or NodePorts.

Because the singleton migration CR may be in a different namespace, it cannot
be an OwnerReference for transfer resources. Label every object with migration
UID and volume-copy identity, and use an explicit cleanup finalizer/state
machine. A namespaced `VolumeCopy` CR may own pods, Services, Secrets and the
temporary target PVC within the workload namespace.

### 7.3 Filesystem fidelity modes

Default restricted mode preserves recursive content, symlinks, permissions,
modification times and hard links, but not arbitrary owners, groups, devices or
special files. This is appropriate only when the destination uses the same
namespace UID/GID model and inventory confirms the data does not require
privileged preservation.

Because a byte-intact copy that loses ownership/xattr/SELinux context is
*functionally* corrupt for apps that depend on them, and inventory cannot
reliably auto-detect this, the controller must not silently *choose* Restricted
for an ambiguous workload. When fidelity requirements cannot be positively
established, default that workload to `Exact` (if the required privileges are
available) or `Manual`, not Restricted.

Regardless of mode:

- use `--sparse` so sparse source files (VM images, preallocated DB files) are
  not inflated on the destination, which would otherwise risk target-space
  blowout or a mid-copy failure;
- enumerate and report skipped special files (devices, sockets, FIFOs) during
  preflight rather than discovering them at copy time.

An `Exact` mode may request the additional SCC/capabilities needed for owner,
group, ACL and xattr preservation. Preflight must report the additional
privileges and must not silently downgrade. The final validation records the
effective rsync options.

## 8. Copy Lifecycle

### 8.1 Prepare target

1. Create a temporary target PVC in the source PVC's namespace with a stable
   generated name, for example `<source>-vcf-target-<hash>`.
2. Size the target from `max(requested capacity, actual used bytes + slack)`,
   not the source PVC request alone: a source filesystem expanded beyond its
   PVC request, or one that is nearly full, would otherwise overflow a
   same-sized target and fail the authoritative rsync mid-copy. Copy access
   modes and filesystem volume mode; replace the StorageClass with the
   configured target class. Never copy `volumeName`, selected-node or
   provisioner binding annotations.
3. Create the receiver with target affinity, causing WFFC provisioning.
4. Wait for PVC Bound, receiver Running, and all destination ownership checks
   in section 6.2.

### 8.2 Optional online precopy

Precopy is enabled per volume only when a read-only reader can be scheduled:

- RWX/ROX: normally eligible while the application remains online.
- RWO: eligible only on the exact node already using the PVC. Kubernetes RWO
  is single-node, not necessarily single-pod.
- RWOP: not eligible while a workload pod owns the volume.
- idle volumes: eligible after a successful source-node mount probe.

Run rsync with `--partial` and without `--delete`. Source files may change or
vanish during this pass; an rsync "vanished source files" result can be recorded
as a non-authoritative precopy outcome. All other errors remain actionable.

The destination contents are the resume checkpoint. A retry reruns rsync
against the same target PVC and transfers only remaining/changed data. No
separate byte-offset database is required for filesystem copies.

### 8.3 Quiesce writers

The authoritative final pass requires zero writers, not necessarily zero
readers.

- Generic automatic mode: scale every owning scalable controller in the
  workload group to zero and record original replica counts. Wait for consumer
  pods to terminate.
- Manual mode: require a ready annotation after the user quiesces the
  application; independently verify no known pod mounts the PVC read-write.
- Application adapters may provide a stronger, less disruptive quiesce. For
  example, the image registry can remain available for pulls in read-only mode.
- Suspend jobs, CronJobs or operators that could recreate writers during the
  window.

For RWO/RWOP, wait for the workload's VolumeAttachment to disappear before
starting the final reader. The reader's own attachment is expected and is not
treated as a failure.

### 8.4 Final sync

Immediately before the pass, assert the destination-≠-source guard (invariant
12): re-verify that the receiver's mounted PV `volumeHandle` differs from the
source PV `volumeHandle` and that `CNSVolumeInfo`/a target CNS query maps it to
the target vCenter. Because the pass uses `--delete`, a misresolved receiver
pointing at the source volume would destroy source data; do not run the pass
unless the guard holds.

Create a fresh reader attempt against the same receiver/target PVC and run an
authoritative rsync with:

- archive-compatible restricted or exact fidelity options;
- `--partial` for retry efficiency;
- `--checksum` for changed-file detection;
- `--delete`/`--delete-delay` so destination state mirrors the frozen source;
- a successful exit code required for progression.

Revalidate zero writers immediately before and after the final pass. If a
writer appears, discard the authoritative marker, quiesce again and repeat the
final pass. The no-writer guarantee must hold continuously from the final pass
through cutover completion, not only during the pass: for RWX keep the owning
controllers scaled to zero / read-only until the PVC binding is switched, since
a writer that starts in the gap would write to the source and be lost at
cutover. A third-party operator that recreates writers and cannot be suspended
makes the workload a `Manual` case.

### 8.5 Cutover strategies

The plan supports three explicit strategies; selection is stored before copy:

1. `Adapter`: a component-specific patch. The image registry patches its
   configured claim and scheduling constraints.
2. `PatchConsumer`: patch supported pod templates from the source claim name to
   the temporary target claim name. Record an inverse merge patch for rollback.
3. `RebindOriginalName`: preserve workload manifests by rebinding the original
   PVC name to the target PV. Each step is gated and persisted so a crash at any
   point leaves the data on a retained, recorded PV (invariants 10–11):
   - delete transfer pods and wait for the receiver's VolumeAttachment(s) to
     disappear, so the target volume is cleanly unmounted and flushed before
     any binding change (rsync success alone does not guarantee durable flush);
   - persist `sourcePV{name,uid,volumeHandle}` and `targetPV{name,uid,
     volumeHandle}` plus a `Rebinding` sub-state marker *before* any destructive
     step;
   - set both source and target PV reclaim policies to `Retain`, then re-GET
     each PV and confirm the `Retain` policy is persisted; do not proceed to any
     PVC delete until confirmed (a PVC delete against a still-`Delete` PV would
     destroy its backing volume);
   - delete the temporary target PVC, then *reserve* the target PV for the new
     claim by setting `PV.spec.claimRef` to the intended PVC
     (namespace + name, with `uid`/`resourceVersion` left empty) rather than
     clearing it — an empty claimRef makes the PV `Available` and racy PVCs
     could bind it;
   - delete the original source PVC, leaving its PV retained (data preserved);
   - recreate the original PVC name with the target StorageClass and
     `spec.volumeName=<targetPV>` (two-way pin with the reserved claimRef);
   - wait for Bound and verify it resolves to the recorded target PV.

Crash-recovery states (reconstructed from cluster objects + `VolumeCopy.status`
on restart):

| State | On-disk signature | Resume action |
|---|---|---|
| `SourceBound` | original PVC bound to source PV | (re)start rebind |
| `PVsRetained` | both PVs `Retain` (verified) | continue to reserve target |
| `TargetReserved` | temp PVC gone, target PV claimRef reserved | delete source PVC |
| `SourcePVCDeleted` | source PVC gone, source PV retained/Released | recreate original PVC |
| `OriginalRecreated` | original PVC exists, `volumeName=targetPV` | wait for Bound |
| `Rebound` | original PVC bound to target PV | resume workload |

`RebindOriginalName` changes the PVC UID and is only attempted while consumers
are stopped. It is useful for StatefulSet-generated claim names and workloads
whose manifests must remain unchanged. If the original name is claimed by an
unexpected UID, stop rather than adopt it.

After cutover, add or retain target topology placement on the workload, resume
it, and verify all pods run on target-vCenter Nodes and mount the expected
target volume handle.

## 9. OpenShift Image Registry Adapter

Detect the configured claim through
`configs.imageregistry.operator.openshift.io/cluster`. For a PVC-backed
registry:

1. Create and validate the target RWX PVC and receiver.
2. Suspend `imagepruners.imageregistry.operator.openshift.io/cluster` and wait
   for any active prune Job to finish.
3. Record internally referenced image digests for post-cutover validation.
4. Run optional online precopy while the registry serves normally.
5. Set registry `spec.readOnly=true`; wait for the operator rollout and ensure
   builds/uploads that write the registry have stopped.
6. Run the authoritative final sync with delete and checksum options.
7. Patch the registry claim to the target PVC and add target-node placement in
   the same desired-state update. Keep read-only enabled.
8. Wait for the image-registry Deployment and ClusterOperator to become
   Available and not Progressing/Degraded. Verify registry pods use the target
   claim and run on target-vCenter Nodes.
9. Resolve or pull every recorded digest through the registry Service.
10. Clear read-only and resume pruning.

The registry is not scaled to zero merely to copy data. RWX permits the reader
mount, and registry read-only mode provides the final no-writer boundary while
allowing pulls. If read-only convergence cannot be proven, fall back to scaling
the registry down for the final pass.

Set storage management to `Unmanaged` before any operation that could cause
the Image Registry Operator to remove managed storage. Never use top-level
`Removed` as the normal quiesce mechanism.

## 10. API and Status

Proposed user-facing fields:

```yaml
spec:
  storage:
    mode: Automatic                 # Automatic | Manual | Skip
    strategy: Auto                  # Auto | Copy | CNSRelay
    maxConcurrentCopies: 2
    preCopy: true
    fidelity: Restricted            # Restricted | Exact
    rollbackRetention: 24h
    storageClassMappings:
      - source: vsan-file-source
        target: vsan-file-target
        targetFailureDomain: target-zone-a
        cutoverStrategy: RebindOriginalName
    transfer:
      image: quay.io/konveyor/rsync-transfer@sha256:<digest>
      bandwidthLimit: ""
      targetTolerations: []
```

Use a namespaced `VolumeCopy` CR for each PVC because copies are long-running,
cross multiple reconciles, and create resources outside the migration CR's
namespace:

```yaml
spec:
  migrationUID: <uid>
  sourcePVC: {name: data, uid: <uid>}
  targetStorageClass: vsan-file-target
  targetFailureDomain: target-zone-a
  preCopy: true
  cutoverStrategy: RebindOriginalName
status:
  phase: Planned | TargetPrepared | PreCopying | PreCopied | Quiescing |
         FinalCopying | Copied | CuttingOver | Validating | Succeeded | Failed
  sourcePlacement: {node, nodeUID, machine, vCenter, region, zone}
  targetPlacement: {node, nodeUID, machine, vCenter, region, zone}
  sourcePV: {name, uid, volumeHandle}
  targetPVC: {name, uid}
  targetPV: {name, uid, volumeHandle}
  attempt: 2
  bytesCopied: 0
  originalWorkloadState: []
  message: ""
```

The top-level migration status contains aggregate counts and references rather
than an unbounded copy of all per-volume detail.

## 11. Reconciliation and Idempotency

Each phase first observes actual state and then performs at most one transition:

- adopt only resources labeled with both migration UID and VolumeCopy UID;
- include source PVC UID/PV UID in identity checks so a same-name replacement
  is never mistaken for the original;
- reuse the same destination PVC across copy retries;
- use stable attempt labels and inspect pod termination status before creating
  another attempt;
- persist quiesce state before scaling or patching a workload;
- persist the completed final-copy attempt before changing PVC bindings;
- persist source-PV and target-PV identity plus the rebind sub-state before any
  destructive cutover step, so a crash cannot orphan the retained fallback PV;
- never clean the source PV automatically during forward reconciliation.

Pausing the migration stops creation of new disruptive transitions. Existing
rsync pods may finish their current pass; the status records the result. A
controller restart resumes from Kubernetes objects and `VolumeCopy.status`, not
from in-memory goroutines or log messages.

## 12. Failure Handling and Rollback

- Target preparation/precopy failure: workload remains on source; delete only
  operator-created target/transfer resources after user acknowledgement.
- Final-copy failure: workload remains quiesced by default so retry does not
  invalidate already-copied data. `resumeOnFailure` may opt into restoring the
  source workload.
- Failure before target writes are enabled: reverse the consumer patch or bind
  the original PVC name back to the retained source PV, then resume.
- Failure after target writes: automatic metadata-only rollback is forbidden;
  data must be frozen and reverse-synced first.
- Unexpected source or target ownership, node placement, PVC UID or PV binding:
  stop with an actionable condition; never guess.

`SourceCleaned` must verify:

- no workload or registry configuration references a source-volume PVC;
- all successful VolumeCopies resolve to target-vCenter volume handles;
- no transfer pods/services/secrets remain;
- no pending rollback uses a source volume;
- the configured rollback-retention period has expired or the user explicitly
  acknowledged source-volume cleanup.

Because retained rebind leaves both the source PV object *and* its backing
CNS/FCD volume in place, cleanup must distinguish the two: deleting only the PV
object leaves the FCD (and its customer data) stranded on the source datastore.
Once the retention/ack gate is satisfied, cleanup deletes the retained source PV
object and explicitly deletes the underlying source-vCenter FCD, and verifies a
source CNS query for the cluster returns zero volumes. Neither deletion may run
before the gate.

## 13. Preflight

When copy migration is selected, validate:

1. The OpenShift release and vSphere CSI operator support the required
   multi-vCenter topology configuration.
2. `MultiSiteConfigured` will expose both vCenters to CSI and `CNSVolumeInfo`
   reconciliation. The ownership-verification path is exercised end to end
   before any destructive step: `CNSVolumeInfo` is present (or a direct CNS
   query is available as fallback), a volume handle resolves to a definite
   vCenter, and its absence/lag is treated as "await and re-verify," never as
   "assume target." Account for the observed 30–60s target-side CNS
   registration lag by polling with a timeout.
2a. The existing storage preflight gates must be reconciled with this feature.
    Today the operator hard-fails when any `csi.vsphere.vmware.com` PV exists
    (`checkNoVSphereCSIPersistentVolumes`) and requires the vSphere
    `ClusterCSIDriver` managementState to be `Removed`
    (`checkVSphereStorageManagement`) — the opposite of what copy migration
    needs (invariant 1: CSI healthy). When storage migration is enabled these
    gates must be relaxed/replaced so CSI runs multi-vCenter during
    `StorageMigrated`; the CSI-Removed expectation applies only to the terminal
    post-migration state, not during copy.
3. Source and target Nodes can be mapped unambiguously through Machines to
   their vCenter servers.
4. Target workers exist or can be created before storage migration, and their
   region/zone labels exactly match the chosen failure domains.
5. Each target StorageClass mapping exists, uses the expected provisioner and
   capability, and is topologically constrained as described in section 6.2.
6. Target capacity is sufficient for the requested PVCs plus configured slack.
7. The pinned transfer image is pullable from source and target nodes.
8. Required SCC/RBAC/NetworkPolicy permissions are available.
9. Workloads have a supported automatic quiesce/cutover adapter or are marked
   `Manual`.
10. Raw block, RWOP, snapshots, ownership/xattr requirements and external
    storage are reported individually rather than hidden behind a generic
    unsupported-volume error.

Missing source PV node affinity is a warning plus an explicit placement/mount
validation path, not a blanket failure. Missing or ambiguous destination
topology is a hard failure.

## 14. RBAC

Add least-privilege access for:

- PVs, PVCs, Pods, Services, Secrets, ConfigMaps, Events and Namespaces;
- StorageClasses, VolumeAttachments and CSIDrivers;
- Nodes and Machine/MachineSet resources required for topology mapping;
- Deployments, StatefulSets, DaemonSets, Jobs and CronJobs for consumer
  discovery/quiesce;
- `CNSVolumeInfo` resources;
- image-registry Config and ImagePruner resources for the registry adapter;
- VolumeCopy CRs and status/finalizers.

PVC namespaces need Roles/RoleBindings for transfer ServiceAccounts. Avoid a
single highly privileged transfer ServiceAccount shared across namespaces.

## 15. Testing Strategy

### Unit tests

- source node selection with and without PV node affinity;
- active RWO exact-node placement, RWX source-node selection and RWOP rejection;
- Node-to-Machine-to-vCenter verification, including duplicate region/zone tag
  values across vCenters;
- target StorageClass validation for WFFC, constrained Immediate and rejected
  unconstrained Immediate modes;
- destination PV/CNSVolumeInfo ownership validation;
- classification matrix and workload grouping;
- rsync option generation for precopy versus final copy;
- all three cutover strategies and inverse rollback transitions;
- registry read-only/pruner state capture and restoration.

### envtest/controller tests

- phase ordering: no destination PVC before MultiSiteConfigured and target
  capacity readiness;
- restart at every VolumeCopy phase;
- stale pod/node/PVC UID rejection;
- writer reappears during final sync and invalidates the authoritative marker;
- cross-namespace transfer resources are cleaned by labels/finalizers;
- pause behavior during precopy, quiesce and cutover.

### e2e tests

In a real dual-vCenter OpenShift environment:

1. RWX vSAN File Services registry PVC with no source PV node affinity:
   online precopy, read-only final delta, claim switch, digest validation.
2. RWO filesystem PVC used by a running pod: same-node precopy and offline
   final delta.
3. RWOP filesystem PVC: offline-only copy.
4. Failure/restart during large precopy and final sync.
5. Target node loss before and after target PVC binding.
6. Duplicate topology label values across vCenters to prove Machine/vCenter
   validation prevents misplacement.
7. Rollback before target writes and guarded refusal after target writes.

Measure total bytes, precopy duration, final-delta duration, application
downtime, CPU usage and source/target datastore throughput. Compare selected
filesystem cases with the staged CNS relay before choosing production defaults.

## 16. Rollout Plan

1. **F1 — topology and inventory:** VolumeCopy API, classification,
   Node/Machine/vCenter mapping and target StorageClass validation.
2. **F2 — resumable data plane:** pin pvc-transfer, add read-only mounts,
   ClusterIP+stunnel endpoint, stable attempts and precopy/final options.
3. **F3 — idle PVC migration:** target preparation, offline final copy,
   validation and retained-source rollback.
4. **F4 — workload lifecycle:** grouping, automatic/manual quiesce,
   `PatchConsumer` and `RebindOriginalName` cutovers.
5. **F5 — image registry:** online precopy, read-only final sync, claim switch
   and digest verification.
6. **F6 — CNS interoperability:** retain CNS relay only for raw block and
   explicit preserve-identity policy; remove the shared-datastore prerequisite
   from copy-only migrations.

## 17. Relationship to the CNS Design

This design supersedes the blanket statement in
`csi-volume-migration.md` that RWX/file volumes always fall back to OADP/MTC.
It does not invalidate the staged CNS relay. The storage planner selects a
backend per volume:

```text
NoMove       storage independent of source vCenter
FileCopy     filesystem data copied to a newly provisioned target volume
CNSRelay     raw block or explicit identity-preserving migration
Manual       application-native or unsupported case
```

The common orchestration layer owns inventory, workload grouping, quiesce,
target topology, status, validation and source-cleanup gates. Movers implement
only backend-specific target preparation and data movement.
