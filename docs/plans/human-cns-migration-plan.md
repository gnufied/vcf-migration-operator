# Customer-Coordinated File-Copy Migration of vSphere CSI Volumes

Status: Draft / Proposal

## 1. Scope

This document defines a first implementation of filesystem-mode vSphere CSI
volume migration between vCenters in the same OpenShift cluster. VMO provisions
new target volumes, copies their data, switches PVC bindings, and verifies
storage placement. Customers remain responsible for stopping, resuming, and
validating their applications at explicit checkpoints.

The unit of coordination is a namespace. All vSphere-backed PVCs and their
consumers in one namespace are prepared, quiesced, cut over, and validated
together. This is intentionally coarse for the first implementation. Preflight
must report that the temporary namespace selector also affects newly created
Pods in that namespace that do not use migrated PVCs.

Raw block migration, the staged CNS relay, and automatic modification of
application controllers are outside the scope of this document. `VolumeCopy`
continues to represent the resumable transfer and cutover of one PVC.

## 2. API ownership

Add `Planning` to `VmwareCloudFoundationMigration.spec.state`. Planning builds
inventory and reports blockers. It may create report CRs, but it must not create
workers, modify vCenter or CSI configuration, label Nodes, provision target
PVCs, start transfer Pods, or change applications.

The storage configuration separates StorageClass selection from failure-domain
selection:

```yaml
spec:
  state: Planning
  storage:
    mode: Manual
    strategy: Copy
    storageClassMappings:
      - source: thin-csi
        target: thin-csi-new
    failureDomainMappings:
      - source: source-a
        target: target-a
      - source: source-b
        target: target-b
      - source: source-c
        target: target-c
```

`storageClassMappings` and `failureDomainMappings` are entries in the migration
CR, not separate Kubernetes objects. One target StorageClass such as
`thin-csi-new` may span every target failure domain. It should use
`WaitForFirstConsumer` and a target storage policy capable of provisioning in
all configured target failure domains.

A customer may override the mapped target failure domain for an individual
source PVC:

```yaml
metadata:
  annotations:
    migration.openshift.io/target-failure-domain: target-b
```

The annotation takes precedence over `failureDomainMappings`. VMO validates
the value against `spec.failureDomains` and records the resolved value in the
immutable `VolumeCopy.spec.targetFailureDomain`. If source topology is missing
or ambiguous and there is no valid override, the namespace remains blocked in
Planning.

## 3. StorageWorkloadMigration

During Planning, VMO creates one `StorageWorkloadMigration` in every namespace
that contains a vSphere-backed PVC. Keeping it in the workload namespace lets
it own the namespace's `VolumeCopy` objects; each `VolumeCopy` can in turn own
its transfer Pods, Service, Secret, NetworkPolicy, and temporary target PVC.
Namespace administrators can update the customer-action fields without access
to the VMO namespace.

Because the top-level migration may be in another namespace, it cannot own the
`StorageWorkloadMigration`. The object carries the migration name and UID, is
labeled with the migration UID, and uses an explicit finalizer for cleanup.

Example:

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: StorageWorkloadMigration
metadata:
  name: vcf-storage-<short-migration-uid>
  namespace: payments
spec:
  migrationRef:
    name: cluster
    uid: <migration-uid>
  actions:
    finalCopyApproved: false
    validationApproved: false
    sourceCleanupApproved: false
status:
  phase: Planning
  namespaceUID: <namespace-uid>
  volumes: []
  consumers: []
  volumeCopies: []
  conditions: []
```

Customers write only `spec.actions`. VMO writes only `status`. VMO must never
advance progress by changing the object's spec.

Planning inventory records object identity, not names alone:

```yaml
status:
  volumes:
    - sourcePVC: {name: data-database-0, uid: <pvc-uid>}
      sourcePV:
        name: pvc-1234
        uid: <pv-uid>
        volumeHandle: <source-fcd-uuid>
      targetStorageClass: thin-csi-new
      targetFailureDomain: target-a
      volumeCopyRef: data-database-0-abc123
  consumers:
    - apiVersion: apps/v1
      kind: StatefulSet
      name: database
      uid: <statefulset-uid>
```

VMO revalidates this inventory when the migration changes to `Running` and
again before each disruptive transition. A new or replaced PVC, PV, Namespace,
or authoritative workload controller invalidates the affected plan and blocks
progress until Planning is repeated. Approval fields never suppress identity,
placement, attachment, or writer checks.

## 4. Site placement fence

While the migration is `Running`, VMO labels Nodes only after positively
mapping each Node through its Machine provider spec `workspace.server`:

```yaml
migration.openshift.io/site: <migration-uid>-source
migration.openshift.io/site: <migration-uid>-target
```

VMO also records and temporarily updates the relevant MachineSet templates so
replacement Nodes receive the correct site label while migration is active.
VMO continually verifies Node UID, Machine identity, vCenter, readiness, and
topology before using a Node. A newly created, unlabeled Node remains ineligible
until VMO classifies it.

The site label has one purpose: distinguish source-vCenter Nodes from
target-vCenter Nodes. It does not select a zone. Region and zone constraints
remain separate.

After storage cutover, VMO temporarily constrains newly created application
Pods in the namespace with:

```yaml
metadata:
  annotations:
    openshift.io/node-selector: >-
      migration.openshift.io/site=<migration-uid>-target
```

Before changing the Namespace, VMO stores a recovery record containing the
Namespace UID, migration UID, whether the annotation originally existed, its
original value, and the exact value VMO applied. Cleanup restores the original
value only when those identities still match and the current selector still
equals the VMO-applied value. A conflicting customer change blocks cleanup
instead of being overwritten.

VMO must not apply this namespace selector before the authoritative final copy.
The final source reader runs in the workload namespace and must still schedule
on a source-vCenter Node.

## 5. Topology rules

### 5.1 Non-zonal clusters

The source reader requires the source site label. The target receiver requires
the target site label. The temporary namespace selector keeps resumed workloads
on the target vCenter while source and target workers coexist.

If the target PV has no useful node affinity, the target site selector remains
the placement safety boundary until source compute is retired.

### 5.2 Zonal clusters

VMO resolves a target failure domain for every PVC using this precedence:

```text
source PVC target-failure-domain annotation
    -> failureDomainMappings[source PV failure domain]
    -> unresolved Planning blocker
```

StorageClass mapping only selects the target StorageClass. It does not select
the receiver's failure domain.

The complete CSI topology tuple must identify exactly one vCenter. Repeating an
individual zone value is acceptable when the complete tuple remains unique:

```text
source: (region=old-vc, zone=zone-a)
target: (region=new-vc, zone=zone-a)
```

Repeating both values across vCenters is ambiguous and blocks Planning:

```text
source: (region=region-1, zone=zone-a)
target: (region=region-1, zone=zone-a)
```

VMO validates topology using both vCenter tag inventory and current
Node -> Machine -> `workspace.server` mappings. A migration site label can
constrain Kubernetes scheduling, but it cannot repair an ambiguous topology
segment presented to the vSphere CSI provisioner.

The source reader requires the source site label plus the resolved source
region and zone. The target receiver requires the target site label plus the
resolved target region and zone. These compact selectors avoid pinning a
receiver to one hostname or listing every eligible Node.

For example:

```yaml
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: migration.openshift.io/site
                operator: In
                values: [<migration-uid>-target]
              - key: topology.kubernetes.io/region
                operator: In
                values: [new-vc]
              - key: topology.kubernetes.io/zone
                operator: In
                values: [zone-a]
```

Every set of PVCs mounted by one Pod must resolve to a compatible target
topology. StatefulSet ordinals may resolve independently when each ordinal has
its own Pod and PVC. An incompatible multi-PVC Pod is a Planning blocker.

The target PV's CSI-generated node affinity is the permanent zonal placement
constraint. The temporary site selector adds protection while source workers
remain and is removed after source compute retirement.

## 6. Target preparation and precopy

After the customer changes the migration to `Running`, VMO follows the normal
pipeline:

```text
InfrastructurePrepared
DestinationInitialized
MultiSiteConfigured
TargetCapacityReady
StorageMigrated
  PrepareTargets
  PreCopy
  AwaitQuiesce
  FinalCopy
  CutOverAndValidate
WorkloadMigrated
SourceCleaned
Ready
```

`TargetCapacityReady` means the required target MachineSets exist and Ready,
schedulable target Nodes are available in every selected target failure domain.

For each PVC, VMO:

1. Resolves and persists the target StorageClass and target failure domain.
2. Creates a temporary target PVC using the broad WFFC target StorageClass.
3. Creates a receiver Pod with the target site constraint and, for zonal
   clusters, the resolved target region and zone.
4. Lets the receiver act as the first consumer that drives target volume
   provisioning.
5. Verifies the receiver Node through Node -> Machine -> vCenter, verifies the
   bound PV topology, confirms target ownership through `CNSVolumeInfo` or a
   target CNS query, and validates capacity and storage policy.
6. Creates a read-only source reader using independently resolved source
   placement and runs the optional, non-authoritative precopy.

Online RWO precopy remains limited to the Node already mounting the volume.
RWOP is not eligible while the application owns the volume. An unavailable or
unsafe precopy does not prevent an offline final copy.

When every eligible PVC in a namespace has completed precopy, VMO sets the
`StorageWorkloadMigration.status.phase` to `AwaitingQuiesce` and emits a
supplemental Event. Status, rather than the Event, is the durable customer
interface.

## 7. Customer quiesce and authoritative final copy

At `AwaitingQuiesce`, the customer:

1. Stops every workload that can mount a source PVC in the namespace.
2. Suspends Jobs, CronJobs, application operators, GitOps reconciliation, or
   other mechanisms that could recreate a writer.
3. Sets `spec.actions.finalCopyApproved: true`.

VMO independently verifies that the recorded workload identities still match,
no Pod has a writable source mount, and required workload VolumeAttachments
have disappeared. For this first implementation, VMO may require all known
consumers to be stopped even when a read-only consumer could theoretically
remain.

VMO then performs one namespace cutover:

1. Run the authoritative final rsync for every PVC while writers remain absent.
2. Recheck for writers before, during, and after the final copies. A writer that
   reappears invalidates final-copy success and returns the namespace to a
   blocked quiesce state.
3. Persist successful final-copy attempts in their `VolumeCopy` statuses.
4. Delete the source readers and target receivers, then wait for their
   VolumeAttachments to disappear so the target filesystems are flushed and
   unmounted before changing any binding.
5. Switch every PVC in the namespace using the selected cutover strategy. A
   same-name rebind must preserve and verify retained source and target PV
   identities before deleting any PVC.
6. Verify every original PVC name or consumer reference resolves to the
   expected target PV and target volume handle.
7. Record the original Namespace selector and apply the temporary target-site
   selector.
8. Set `StorageWorkloadMigration.status.phase` to `ReadyForResume`.

`DataCopied` is not a resume boundary. Resume is allowed only after data copy,
PVC cutover, binding verification, transfer cleanup, and target placement are
all complete.

## 8. Resume and validation

At `ReadyForResume`, VMO emits a supplemental Event asking the customer to
resume the namespace's applications. The customer resumes them through their
authoritative configuration, not by editing generated Pods or controllers.

VMO sets the phase to `Validating` and verifies that:

- application Pods run only on Nodes positively mapped to the target vCenter;
- zonal Pods run in a topology allowed by every mounted target PV;
- mounted CSI volume handles equal the recorded target handles;
- no application configuration references a temporary or retained source PVC;
- the expected workloads become ready; and
- no source volume is attached to an application Pod.

The customer performs application-level validation and then sets
`spec.actions.validationApproved: true`. VMO revalidates the observable checks
and sets the phase to `Complete`.

The operator repeats this lifecycle for each affected namespace. A blocked
namespace does not lose its source data and prevents source cleanup. When all
`StorageWorkloadMigration` objects are `Complete`, the top-level migration may
continue `WorkloadMigrated` and retire source workers.

## 9. Placement cleanup

Placement cleanup is separate from source-data deletion. VMO automatically
releases the temporary placement fence after it verifies that:

- source worker MachineSets cannot recreate source workers;
- no schedulable source-vCenter worker Nodes remain;
- all namespace migrations are `Complete`; and
- no active rollback requires source placement.

VMO then:

1. Restores each Namespace's original `openshift.io/node-selector` value using
   its recovery record.
2. Recreates any Pending application Pods that still contain an injected stale
   migration selector when necessary.
3. Removes migration site labels from target MachineSet templates and Nodes.
4. Records placement cleanup as complete.

Future target Nodes do not need migration labels after this point. Retained
source volumes may remain for the rollback-retention period without keeping
application scheduling constrained.

## 10. Source-data cleanup

Source-data cleanup is destructive and requires a separate customer approval.
For each namespace, VMO waits until:

- the namespace migration is `Complete`;
- application validation succeeded;
- the rollback-retention period expired, unless an existing explicit early
  cleanup acknowledgement applies;
- no rollback operation references a source volume; and
- the customer sets `spec.actions.sourceCleanupApproved: true`.

VMO displays the source PV names, UIDs, volume handles, and vCenter ownership in
status before accepting approval. Cleanup deletes both the retained source PV
object and its underlying source-vCenter FCD, then verifies through a source
CNS query that the volume no longer exists. Deleting only the PV object is not
sufficient.

`SourceCleaned` becomes true only after every source volume is removed and no
transfer or placement-cleanup resources remain. Until then, the migration may
remain operationally complete but cannot become `Ready`.

## 11. Failure behavior

- A Planning or target-preparation failure leaves applications running on
  source storage.
- A precopy failure leaves the source authoritative and may be retried.
- A final-copy or cutover failure leaves the namespace quiesced by default and
  preserves both recorded source and target data identities.
- A pause preserves transfer checkpoints, quiescence, and placement fences.
- Before target writes are enabled, rollback may restore source bindings and
  source placement before allowing applications to resume.
- After target writes are enabled, metadata-only rollback is forbidden. The
  workload must be frozen again and data reverse-synchronized, or recovered by
  an application-specific manual procedure.
- VMO never adopts a same-name replacement PVC, PV, Namespace, or controller
  when its recorded UID differs.
