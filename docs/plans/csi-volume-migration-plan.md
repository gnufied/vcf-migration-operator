# Customer-Coordinated File-Copy Migration of vSphere CSI Volumes

Status: Draft / Proposal

## 1\. Scope

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

## 2\. API Changes

### 1\. Changes to VmwareCloudFoundationMigration type:

Add `Planning` to `VmwareCloudFoundationMigration.spec.state`. When migration is in planning phase VMO will build inventory of volumes to migrate, create any blockers if volume can't be placed on target vCenter and in-general let customers know about volumes that will be migrated via custom `StorageWorkloadMigration` CR.

The storage configuration separates StorageClass selection from failure-domain selection and below is final shape of `VmwareCloudFoundationMigration` object:

```
spec:
  state: Planning
  storage:
    # Whether to cleanup Source data
    cleanupSourceData: <true|false>
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

`storageClassMappings` and `failureDomainMappings` are entries in the migration CR, not separate Kubernetes objects. One target StorageClass such as `thin-csi-new` may span every target failure domain. It should use `WaitForFirstConsumer` and a target storage policy capable of provisioning in all configured target failure domains.

A customer may override the mapped target failure domain for an individual source PVC by adding below annotation:

```
metadata:
  annotations:
    migration.openshift.io/target-failure-domain: target-b
```

The annotation takes precedence over `failureDomainMappings`. VMO validates the value against `spec.failureDomains`. During Planning, VMO records the resolved StorageClass and failure domain in `StorageWorkloadMigration.status.volumes[].targetStorageClass` and `targetFailureDomain`. Before target preparation starts, VMO copies those values into the corresponding immutable `VolumeCopy.spec.targetStorageClass` and `VolumeCopy.spec.targetFailureDomain`. If source topology is missing or ambiguous and there is no valid override, the namespace remains blocked in Planning.

### 2\. VolumeCopy object

Since data migration can take long time, we track migration of each PVC via corresponding `VolumeCopy` object:

```
type VolumeCopy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeCopySpec   `json:"spec"`
	Status VolumeCopyStatus `json:"status,omitempty"`
}
```

## 3\. StorageWorkloadMigration

During Planning, VMO creates one `StorageWorkloadMigration` in every namespace that contains a vSphere-backed PVC. Keeping it in the workload namespace lets it own the namespace's `VolumeCopy` objects; each `VolumeCopy` can in turn own its transfer Pods, temporary target PVC etc.

Example:

```
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
  conditions: []
```

Customers write only `spec.actions`. VMO writes only `status`. VMO must never advance progress by changing the object's spec.

`status.phase` uses the following ordered values:

| Phase | Meaning |
| :---- | :---- |
| `Planning` | VMO inventories the namespace, resolves targets, and reports blockers. |
| `PreparingTargets` | VMO creates and verifies the target PVCs, PVs, receiver Pods, and target ownership. |
| `PreCopying` | VMO runs optional, non-authoritative seed copies. This phase may be skipped. |
| `AwaitingQuiesce` | VMO waits for the customer to stop all consumers and approve the final copy. |
| `FinalCopying` | VMO verifies quiescence and runs the authoritative copies. |
| `CuttingOver` | VMO removes transfer attachments and rebinds the original PVC names to the target PVs. |
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

```
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

VMO revalidates this inventory when the migration changes to `Running` and again before each disruptive transition. A new or replaced PVC, PV, Namespace, or authoritative workload controller invalidates the affected plan and blocks progress until Planning is repeated. Approval fields never suppress identity, placement, attachment, or writer checks.

## 4\. Site placement fence

While the migration is `Running`, VMO labels Nodes only after positively mapping each Node through its Machine provider spec `workspace.server`:

```
migration.openshift.io/site: <migration-uid>-source
migration.openshift.io/site: <migration-uid>-target
```

VMO also records and temporarily updates the relevant *MachineSet* templates so replacement Nodes receive the correct site label while migration is active. VMO continually verifies Node UID, Machine identity, vCenter, readiness, and topology before using a Node. A newly created, unlabeled Node remains ineligible until VMO classifies it.

The site label has one purpose: distinguish source-vCenter Nodes from target-vCenter Nodes. It does not select a zone. Region and zone constraints remain separate.

During Planning, VMO records the Namespace UID, whether `openshift.io/node-selector` exists, and its original value in `StorageWorkloadMigration.status`. VMO does not modify the Namespace.

After all associated PVCs to a namespace are copied , VMO publishes the required target-site selector in status and emits a supplemental Event asking the customer to add it to the Namespace:

```
metadata:
  annotations:
    openshift.io/node-selector: >-
      migration.openshift.io/site=<migration-uid>-target
```

If the Namespace already has a selector, the customer merges the target-site requirement with the existing value rather than replacing unrelated scheduling constraints. VMO observes the resulting annotation, records the applied value, and verifies that it includes the required target-site constraint and remains compatible with the migrated workloads.

VMO must not request this namespace change before the authoritative final copy. The final source reader runs in the workload namespace and must still schedule on a source-vCenter Node. VMO does not publish `ReadyForResume` until it observes and validates the customer-applied selector.

## 5\. Topology rules

### 5.1 Non-zonal clusters

The source reader requires the source site label. The target receiver requires the target site label. The temporary namespace selector keeps resumed workloads on the target vCenter while source and target workers coexist.

If the target PV has no useful node affinity, the target site selector remains the placement safety boundary until source compute is retired.

### 5.2 Zonal clusters

VMO resolves a target failure domain for every PVC using this precedence:

```
source PVC target-failure-domain annotation
    -> failureDomainMappings[source PV failure domain]
    -> unresolved Planning blocker
```

if a PVC's failure-domain can not be resolved during `Planning` phase it blocks migration with following values in `StorageWorkloadMigration`.

```
status:
  conditions:
    - type: Ready
      status: "False"
      reason: TargetFailureDomainUnresolved
      message: >-
        PVC payments/data has no target failure-domain override and source
        failure domain source-a has no mapping

```

StorageClass mapping only selects the target StorageClass. It does not select the receiver's failure domain.

VMO uses both topology and `migration.openshift.io/site` selector to place source(rsync server) and target (rsync target) pods during migration.

For example:

```
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

## 6\. Target preparation and precopy

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

For each PVC, VMO:

1. Revalidates the planned target StorageClass and failure domain, then persists them in `VolumeCopy.spec.targetStorageClass` and `VolumeCopy.spec.targetFailureDomain` for execution.  
2. Creates a temporary target PVC using the broad `WaitForFirstConsumer` target StorageClass.  
3. Creates a receiver Pod with the target site constraint and, for zonal clusters, the resolved target region and zone.  
4. Let the receiver act as the first consumer that drives target volume provisioning.  
5. Verifies the receiver Node through Node \-\> Machine \-\> vCenter, verifies the bound PV topology, confirms target ownership through `CNSVolumeInfo` or a target CNS query, and validates capacity and storage policy.  
6. Creates a read-only source reader using independently resolved source placement and runs the optional, non-authoritative precopy.

Online RWO precopy remains limited to the Node already mounting the volume. RWOP is not eligible while the application owns the volume. An unavailable or unsafe precopy does not prevent an offline final copy.

When every eligible PVC in a namespace has completed precopy, VMO sets the `StorageWorkloadMigration.status.phase` to `AwaitingQuiesce` and emits a supplemental Event. Status, rather than the Event, is the durable customer interface.

## 7\. Customer quiesce and authoritative final copy

At `AwaitingQuiesce`, the customer:

1. Stops every workload that can mount a source PVC in the namespace.  
2. Suspends Jobs, CronJobs, application operators, GitOps reconciliation, or other mechanisms that could recreate a writer.  
3. Sets `spec.actions.finalCopyApproved: true`.

VMO independently verifies that the recorded workload identities still match, no Pod has a writable source mount, and required workload VolumeAttachments have disappeared. For this first implementation, VMO may require all known consumers to be stopped even when a read-only consumer could theoretically remain.

VMO then performs one namespace cutover:

1. Run the authoritative final rsync for every PVC while writers remain absent.  
2. Recheck for writers before, during, and after the final copies. A writer that reappears invalidates final-copy success and returns the namespace to a blocked quiesce state.  
3. Persist successful final-copy attempts in their `VolumeCopy` statuses.  
4. Delete the source readers and target receivers, then wait for their VolumeAttachments to disappear so the target filesystems are flushed and unmounted before changing any binding.  
5. Switch every PVC in the namespace using the selected cutover strategy(In this case rebinding same PVC to new PV). A same-name rebind must preserve and verify retained source and target PV identities before deleting any PVC.  
6. Verify every original PVC name or consumer reference resolves to the expected target PV and target volume handle.  
7. Set `StorageWorkloadMigration.status.phase` to `AwaitingPlacementUpdate`, publish the required selector, and ask the customer to add it to the Namespace.  
8. Observe and validate the customer-applied Namespace selector.  
9. Set `StorageWorkloadMigration.status.phase` to `ReadyForResume`.

## 8\. Resume and validation

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

## 9\. Placement cleanup

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
- Before target writes are enabled, rollback may restore source bindings and source placement before allowing applications to resume.  
- After target writes are enabled, metadata-only rollback is forbidden. The workload must be frozen again and data reverse-synchronized, or recovered by an application-specific manual procedure.  
- VMO never adopts a same-name replacement PVC, PV, Namespace, or controller when its recorded UID differs.
