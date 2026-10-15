# Customer-Coordinated File-Copy Migration of vSphere CSI Volumes

Status: Draft / Proposal

## 1. Scope

VMO copies CNS volumes to new datastores in new vCenter environment using `pvc-transfer` as a library.

Customer is responsible for final quescing of workloads and managing workload placement on new environment.

Migration is performed on per-namespace basis with VMO doing precopy, copy, rebinding etc. VMO will also delete data from source datastore with customer confirmation.

## Goals:

1. VMO copies target CNS volumes using github.com/migtools/pvc-transfer as a library to new vCenter.  
2. It should be possible to replace rsync with something like `CnsRelocateVolume` in future, when it becomes feasible to move CNS volumes between vCenters.   
3. Can we estimate the downtime required for this?  
4. Keep this TP in 5.1 

## Non-Goals

1. Quescing customer workloads.  
2. Modifying customer workload to fit new topology.  
3. Moving raw block volumes  
   1. There was only one raw block PV in all of telemetry  
4. Copying of Snapshot data.

## 2. API Changes

### 1. Changes to VmwareCloudFoundationMigration type:

Add `Planning` to `VmwareCloudFoundationMigration.spec.state`. When migration is in planning phase VMO will build inventory of volumes to migrate, create any blockers if volume can't be placed on target vCenter and in-general let customers know about volumes that will be migrated via custom `StorageWorkloadMigration` CR.

Below is the final shape of the `VmwareCloudFoundationMigration` storage configuration:

```yaml
spec:
  state: Planning
  storage:
    # Whether to cleanup Source data
    cleanupSourceData: <true|false>
    # Volume mover used to copy data (see "Volume movers"). Defaults to Auto.
    dataMigrationStrategy: <Auto|Rsync>
    failureDomainMappings:
      - source: source-a
        target: target-a
      - source: source-b
        target: target-b
      - source: source-c
        target: target-c
```

`failureDomainMappings` are entries in the migration CR, not separate Kubernetes objects.

There is no StorageClass mapping: each target PVC uses the same StorageClass as its source PVC. When VMO adds the target vCenter, the CSI driver operator creates the same-named storage policy there and tags the target failure-domain datastores, so StorageClasses such as `thin-csi` span both vCenters. Target placement comes from the receiver's topology (section 5). For customer-created StorageClasses, the customer must create a same-named storage policy in the target vCenter. Planning blocks a PVC whose StorageClass:

- references a storage policy that is missing in the target vCenter or matches no datastore in the resolved target failure domain;
- uses `Immediate` binding, because only `WaitForFirstConsumer` lets the receiver's topology select the target vCenter; or
- is pinned to the source with `datastoreurl` or `allowedTopologies`.

A customer may override the mapped target failure domain for an individual source PVC by adding below annotation:

```yaml
metadata:
  annotations:
    migration.openshift.io/target-failure-domain: target-b
```

The annotation takes precedence over `failureDomainMappings`. VMO validates the value against `spec.failureDomains`. During Planning, VMO records the resolved failure domain in `StorageWorkloadMigration.status.volumes[].targetFailureDomain`. Before target preparation starts, VMO copies it into the immutable `VolumeCopy.spec.targetFailureDomain`. If source topology is missing or ambiguous and there is no valid override, the namespace remains blocked in Planning.

### 2. VolumeCopy object

Since data migration can take long time, we track migration of each PVC via corresponding `VolumeCopy` object:

```go
type VolumeCopy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeCopySpec   `json:"spec"`
	Status VolumeCopyStatus `json:"status,omitempty"`
}
```

`VolumeCopy.spec.method` records the volume mover resolved during Planning and is immutable, like `targetFailureDomain`. Mover-specific progress lives in a per-method field of `VolumeCopy.status` (for example `status.rsync`).


### 3. Volume movers

The customer-facing lifecycle in this document is the same for every copy mechanism. Only the step that produces a target-vCenter volume with the source's data at the quiesce point is pluggable. VMO calls it through a `VolumeMover` interface:

```go
type VolumeMover interface {
	Method() migrationv1alpha1.VolumeCopyMethod
	Capabilities() MoverCapabilities

	// Validate runs during Planning and returns blockers. It never mutates anything.
	Validate(ctx context.Context, plan VolumePlan) []Blocker

	Prepare(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	PreCopy(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	FinalCopy(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	// Release removes mover-owned Pods and attachments before cutover.
	Release(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (StepResult, error)
	// TargetVolume returns the target volume handle, capacity, and node affinity used by cutover.
	TargetVolume(ctx context.Context, vc *migrationv1alpha1.VolumeCopy) (TargetVolumeRef, error)
}

type MoverCapabilities struct {
	SupportsPreCopy bool // incremental, non-authoritative seed copies
	ConsumesSource  bool // the source FCD no longer exists after FinalCopy
	VolumeModes     []corev1.PersistentVolumeMode
	AccessModes     []corev1.PersistentVolumeAccessMode
}
```

Movers are level-triggered and idempotent, and persist all progress (including any vCenter task reference) in `VolumeCopy.status` so they can resume after an operator restart. Cutover, validation, placement, and cleanup consume only `TargetVolumeRef` and do not depend on the mover.

- **`Rsync`** (this release) uses `pvc-transfer` to copy files between a source reader Pod and a target receiver Pod. It supports precopy and does not consume the source. Sections 5 and 6 describe its Pod placement and target provisioning.
- **`CNSRelocate`** (future) would move the FCD to a target-vCenter datastore with a vSphere API such as `CnsRelocateVolume`, then statically create the target PV from the returned handle. It would have no transfer Pods or precopy, and it would consume the source.

`dataMigrationStrategy: Auto` picks the best mover whose `Validate` passes for each volume, preferring movers that avoid a file-level copy. A mover can only be swapped before `Prepare` starts; after that, changing method requires an explicit reset of the `VolumeCopy`.

A mover with `ConsumesSource` cannot retain source data, so it is only eligible when `cleanupSourceData: true`. An explicit `dataMigrationStrategy` that consumes the source combined with `cleanupSourceData: false` is rejected by API validation, and `Auto` never selects such a mover in that case. Even with `cleanupSourceData: true`, a consuming mover destroys the source at `FinalCopying` rather than after validation and the rollback-retention period. For those volumes `finalCopyApproved` therefore also acts as the source-cleanup approval: rollback to source bindings is impossible, and the retention period and `sourceCleanupApproved` do not apply. VMO reports this per volume in `StorageWorkloadMigration.status` during Planning, so the customer knows before approving the final copy.

## 3. StorageWorkloadMigration

During Planning, VMO creates one `StorageWorkloadMigration` in every namespace that contains a vSphere-backed PVC. Keeping it in the workload namespace lets it own the namespace's `VolumeCopy` objects; each `VolumeCopy` can in turn own its transfer Pods, temporary target PVC etc.

Example:

```go
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
  volumes: []
  consumers: []
  conditions: []
```

Customers write only `spec.actions`. VMO writes only `status`. VMO must never advance progress by changing the object's spec.

`status.phase` uses the following ordered values:

| Phase | Meaning |
| :---- | :---- |
| `Planning` | VMO inventories the namespace, resolves targets, and reports blockers. |
| `PreparingTargets` | The volume mover prepares and VMO verifies target volumes and target ownership. |
| `PreCopying` | The volume mover runs optional, non-authoritative seed copies. Skipped if the mover does not support precopy. |
| `AwaitingQuiesce` | VMO waits for the customer to stop all consumers and approve the final copy. |
| `FinalCopying` | VMO verifies quiescence and the volume mover runs the authoritative copies. |
| `CuttingOver` | The volume mover releases its attachments and VMO rebinds the original PVC names to the target PVs. |
| `AwaitingPlacementUpdate` | VMO waits for the customer to add the published target-site selector to the Namespace. |
| `ReadyForResume` | Storage and placement are ready and VMO asks the customer to resume the workloads. |
| `Validating` | VMO validates the resumed workloads while the customer performs application validation. |
| `WorkloadComplete` | Storage cutover and workload validation are complete; source compute retirement may proceed. |
| `AwaitingPlacementCleanup` | Source workers are gone and VMO waits for the customer to remove the temporary Namespace selector. |
| `CleaningPlacement` | VMO verifies the Namespace change and removes migration labels from Nodes and MachineSets. |
| `AwaitingSourceCleanup` | VMO waits for rollback retention and explicit source-cleanup approval. |
| `CleaningSource` | VMO deletes retained source PVs and source-vCenter FCDs and verifies their removal. |
| `Succeeded` | Workload migration and all required cleanup are complete. |
| `Failed` | An unrecoverable error requires an explicit recovery action. Retriable waits remain in their current phase with conditions describing the problem. |

`status.conditions` reports readiness, blockers, retries, and customer actions within the current phase. A Planning blocker therefore leaves `phase: Planning` and sets an explanatory condition; it does not change the phase to `Blocked`.

Planning inventory records object identity, not names alone:

```go
status:
  volumes:
    - sourcePVC: {name: data-database-0, uid: <pvc-uid>}
      sourcePV:
        name: pvc-1234
        uid: <pv-uid>
        volumeHandle: <source-fcd-uuid>
      targetFailureDomain: target-a
  consumers:
    - apiVersion: apps/v1
      kind: StatefulSet
      name: database
      uid: <statefulset-uid>
```

VMO revalidates this inventory when the migration changes to `Running` and again before each disruptive transition. A new or replaced PVC or PV invalidates the affected plan and blocks progress until Planning is repeated.

`status.consumers` is informational. It lists the workload controllers observed consuming the namespace's PVCs, so the customer knows what to stop and resume, and VMO knows which workloads to check for readiness during `Validating`. A changed or replaced consumer does not block progress; safety relies on PVC and PV identities and on the Pod and VolumeAttachment checks. Approval fields never suppress identity, placement, attachment, or writer checks.

## 4. Site placement fence

While the migration is `Running`, VMO labels Nodes only after positively mapping each Node through its Machine provider spec `workspace.server`:

```go
migration.openshift.io/site: <migration-uid>-source
migration.openshift.io/site: <migration-uid>-target
```

VMO also records and temporarily updates the relevant *MachineSet* templates so replacement Nodes receive the correct site label while migration is active. VMO continually verifies Node UID, Machine identity, vCenter, readiness, and topology before using a Node. A newly created, unlabeled Node remains ineligible until VMO classifies it.

The site label has one purpose: distinguish source-vCenter Nodes from target-vCenter Nodes. It does not select a zone. Region and zone constraints remain separate.

During Planning, VMO records whether `openshift.io/node-selector` exists, and its original value in `StorageWorkloadMigration.status`. VMO does not modify the Namespace.

After all associated PVCs to a namespace are copied , VMO publishes the required target-site selector in status and emits a supplemental Event asking the customer to add it to the Namespace:

```yaml
metadata:
  annotations:
    openshift.io/node-selector: >-
      migration.openshift.io/site=<migration-uid>-target
```

If the Namespace already has a selector, the customer merges the target-site requirement with the existing value rather than replacing unrelated scheduling constraints. VMO observes the resulting annotation, records the applied value, and verifies that it includes the required target-site constraint and remains compatible with the migrated workloads.

VMO must not request this namespace change before the authoritative final copy. The final source reader runs in the workload namespace and must still schedule on a source-vCenter Node. VMO does not publish `ReadyForResume` until it observes and validates the customer-applied selector.

## 5. Topology rules

### 5.1 Non-zonal clusters

The source reader requires the source site label. The target receiver requires the target site label. The temporary namespace selector keeps resumed workloads on the target vCenter while source and target workers coexist.

While source and target coexist, Infrastructure has more than one failure domain, so the CSI driver runs with topology enabled and every target PV gets `topology.csi.vmware.com/openshift-{region,zone}` node affinity.

### 5.2 Keeping CSI topology after source removal

The CSI operator enables topology only when Infrastructure has more than one failure domain, or when `ClusterCSIDriver.spec.driverConfig.vSphere.topologyCategories` is set. If the target has a single failure domain, removing the source failure domains would disable topology. New Nodes would then lack the topology labels that migrated PVs require, and those PVs would become unschedulable as workers are replaced.

This only applies when the target has a single failure domain; with more, the Infrastructure-derived categories take precedence. In that case, before removing source failure domains from Infrastructure, VMO sets `topologyCategories: [openshift-zone, openshift-region]` on the `ClusterCSIDriver` if it is unset. This is a supported field (used by UPI zonal clusters), not a workaround.

- If `topologyCategories` is already set to a different value, Planning reports a blocker: those categories would take over after source removal and no longer match the PVs' node affinity.
- Because the value equals the Infrastructure-derived one, the effective categories never change. VMO verifies that the CSI controller Deployment's arguments are unchanged after source failure domains are removed, so no CSI Pod restarts and no Node loses topology.
- `openshift-region`/`openshift-zone` tags must be on the target datacenter and vSphere cluster (which new hosts inherit), not on individual hosts. VMO verifies this before the change.
- Clearing the field after migration silently strands migrated PVs as Nodes are replaced. The release notes must call this out; an alert for "PVs with `topology.csi.vmware.com` affinity exist while CSI topology is disabled" is a follow-up.

### 5.3 Zonal clusters

VMO resolves a target failure domain for every PVC using this precedence:

```
source PVC target-failure-domain annotation
    -> failureDomainMappings[source PV failure domain]
    -> the only target failure domain, if spec.failureDomains has exactly one
    -> unresolved Planning blocker
```

A mapping is therefore only required when the target has more than one failure domain.

if a PVC's failure-domain can not be resolved during `Planning` phase it blocks migration with following values in `StorageWorkloadMigration`.

```yaml
status:
  conditions:
    - type: Ready
      status: "False"
      reason: TargetFailureDomainUnresolved
      message: >-
        PVC payments/data has no target failure-domain override and source
        failure domain source-a has no mapping

```

The StorageClass does not select the receiver's failure domain.

VMO uses both topology and `migration.openshift.io/site` selector to place source(rsync server) and target (rsync target) pods during migration.

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

Every set of PVCs mounted by one Pod must resolve to a compatible target topology. StatefulSet ordinals may resolve independently when each ordinal has its own Pod and PVC. An incompatible multi-PVC Pod is a Planning blocker.

The target PV's CSI-generated node affinity is the permanent zonal placement constraint. The temporary site selector adds protection while source workers remain and is removed after source compute retirement.

## 6. Target preparation and precopy

After the customer changes the migration to `Running`, VMO follows the normal pipeline:

```
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

`TargetCapacityReady` means the required target MachineSets exist and Ready, schedulable target Nodes are available in every selected target failure domain.

For each PVC, VMO revalidates the planned target failure domain and mover, then persists them in `VolumeCopy.spec` for execution. With the `Rsync` mover, VMO then:

1. Creates a temporary target PVC using the source PVC's `WaitForFirstConsumer` StorageClass.  
2. Creates a receiver Pod with the target site constraint and, for zonal clusters, the resolved target region and zone.  
3. Lets the receiver act as the first consumer that drives target volume provisioning.  
4. Verifies the receiver Node through Node \-\> Machine \-\> vCenter, verifies the bound PV topology, confirms target ownership through `CNSVolumeInfo` or a target CNS query, and validates capacity and storage policy.  
5. Creates a read-only source reader using independently resolved source placement and runs the optional, non-authoritative precopy.

Online RWO precopy remains limited to the Node already mounting the volume. RWOP is not eligible while the application owns the volume. An unavailable or unsafe precopy does not prevent an offline final copy.

When every eligible PVC in a namespace has completed precopy, VMO sets the `StorageWorkloadMigration.status.phase` to `AwaitingQuiesce` and emits a supplemental Event. Status, rather than the Event, is the durable customer interface.

## 7. Customer quiesce and authoritative final copy

At `AwaitingQuiesce`, the customer:

1. Stops every workload that can mount a source PVC in the namespace.  
2. Suspends Jobs, CronJobs, application operators, GitOps reconciliation, or other mechanisms that could recreate a writer.  
3. Sets `spec.actions.finalCopyApproved: true`.

VMO independently verifies that no Pod has a writable source mount, and required workload VolumeAttachments have disappeared. For this first implementation, VMO may require all known consumers to be stopped even when a read-only consumer could theoretically remain.

VMO then performs one namespace cutover:

1. Run the mover's authoritative final copy for every PVC while writers remain absent.  
2. Recheck for writers before, during, and after the final copies. A writer that reappears invalidates final-copy success and returns the namespace to a blocked quiesce state.  
3. Persist successful final-copy attempts in their `VolumeCopy` statuses.  
4. Release mover resources. For `Rsync`, delete the source readers and target receivers, then wait for their VolumeAttachments to disappear so the target filesystems are flushed and unmounted before changing any binding.  
5. Switch every PVC in the namespace to the mover's `TargetVolumeRef` using the selected cutover strategy (in this case rebinding the same PVC name to the new PV). A same-name rebind must preserve and verify retained source and target PV identities before deleting any PVC.  
6. Verify every original PVC name or consumer reference resolves to the expected target PV and target volume handle.  
7. Set `StorageWorkloadMigration.status.phase` to `AwaitingPlacementUpdate`, publish the required selector, and ask the customer to add it to the Namespace.  
8. Observe and validate the customer-applied Namespace selector.  
9. Set `StorageWorkloadMigration.status.phase` to `ReadyForResume`.

## 8. Resume and validation

At `ReadyForResume`, VMO emits a supplemental Event asking the customer to resume the namespace's applications. The customer resumes them through their authoritative configuration, not by editing generated Pods or controllers.

VMO sets the phase to `Validating` and verifies that:

- application Pods run only on Nodes positively mapped to the target vCenter;  
- zonal Pods run in a topology allowed by every mounted target PV;  
- mounted CSI volume handles equal the recorded target handles;  
- no application configuration references a temporary or retained source PVC;  
- the expected workloads become ready; and  
- no source volume is attached to an application Pod.

The customer performs application-level validation and then sets `spec.actions.validationApproved: true`. VMO revalidates the observable checks and sets the phase to `WorkloadComplete`.

The operator repeats this lifecycle for each affected namespace. A blocked namespace does not lose its source data and prevents source cleanup. When all `StorageWorkloadMigration` objects are `WorkloadComplete`, the top-level migration may continue `WorkloadMigrated` and retire source workers.

## 9. Placement cleanup

At placement cleanup we should remove the namespace selectors which were previously used to schedule customer's workloads on target vCenter. We need to remove them because otherwise pod scheduling will forever depend on them.

- source worker MachineSets cannot recreate source workers;  
- no schedulable source-vCenter worker Nodes remain;  
- all namespace migrations reached `WorkloadComplete`; and  
- no active rollback requires source placement,

it sets each affected `StorageWorkloadMigration.status.phase` to `AwaitingPlacementCleanup`, publishes the recorded original selector, and emits a supplemental Event asking the customer to remove the migration site constraint. The customer may restore the recorded original value or apply a different final selector that is valid for the target cluster.

The customer then:

1. Removes `migration.openshift.io/site=<migration-uid>-target` from each Namespace's `openshift.io/node-selector` annotation.  
2. Restores the previous selector or installs the intended final target selector.  
3. Deletes and recreates any Pending application Pods that still contain the injected migration selector.

VMO verifies that the migration selector is absent and that the resulting selector remains compatible with target placement. VMO sets the phase to `CleaningPlacement`, removes migration site labels from the MachineSet templates and Nodes, and then advances to `AwaitingSourceCleanup`. VMO never writes the Namespace selector itself.

Future target Nodes do not need migration labels after this point. Retained source volumes may remain for the rollback-retention period without keeping application scheduling constrained.

## 10\. Source-data cleanup

If customer has opted to cleanup the source via `cleanupSourceData` via `VmwareCloudFoundationMigration` object then VMO will optionally cleanup the data from source datastore.

Movers with `ConsumesSource` require `cleanupSourceData: true` and remove the source during `FinalCopying` (see "Volume movers"), so those volumes have nothing left to clean up here.

If customer has not opted in to cleanup source data, each `StorageWorkloadMigration` is set to `Succeeded` and storage part of migration is considered complete.

Source-data cleanup is destructive and requires a separate customer approval. For each namespace, VMO waits until:

- the namespace reached `AwaitingSourceCleanup`;  
- application validation succeeded;  
- the rollback-retention period expired, unless an existing explicit early cleanup acknowledgement applies;  
- no rollback operation references a source volume; and  
- the customer sets `spec.actions.sourceCleanupApproved: true`.

VMO displays the source PV names, UIDs, volume handles, and vCenter ownership in status before accepting approval. It then sets the phase to `CleaningSource`. Cleanup deletes both the retained source PV object and its underlying source-vCenter FCD, then verifies through a source CNS query that the volume no longer exists. Deleting only the PV object is not sufficient. After every source volume for the namespace is gone, VMO sets the phase to `Succeeded`.

`SourceCleaned` becomes true only after every source volume is removed and no transfer or placement-cleanup resources remain. Until then, the migration may remain operationally complete but cannot become `Ready`.

## 11\. Failure behavior

- A Planning or target-preparation failure leaves applications running on source storage.  
- A precopy failure leaves the source authoritative and may be retried.  
- A final-copy or cutover failure leaves the namespace quiesced by default and preserves both recorded source and target data identities.  
- A pause preserves transfer checkpoints, quiescence, and placement fences.  
- Before target writes are enabled, rollback may restore source bindings and source placement before allowing applications to resume. This does not apply to movers with `ConsumesSource`.  
- VMO never falls back to a different mover after `Prepare` has started for a volume.  
- After target writes are enabled, metadata-only rollback is forbidden. The workload must be frozen again and data reverse-synchronized, or recovered by an application-specific manual procedure.  
- VMO never adopts a same-name replacement PVC or PV when its recorded UID differs.
