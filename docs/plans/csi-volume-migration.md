# CSI (CNS) Volume Migration Across vCenters — Design Plan

Status: Draft / Proposal (Phase 2 — Storage)
Tracking: SPLAT-2644 (epic), enhancement openshift/enhancements#1961 (Phase 1 declares
CSI volume migration a non-goal and fails preflight when vSphere CSI PVs are detected)

> **Revision 2026-08-13**: Broadcom support (case filed after the spike, see
> §8.1) states that **direct datastore-to-datastore migration of CNS volumes
> across vCenters is unsupported** at this time. Approach A (direct cross-VC
> `CnsRelocateVolume`) is therefore demoted from the design basis to an
> opportunistic fast path. The revised primary design is the **staged
> shared-datastore relay** (§4.3a): intra-VC relocate onto a datastore mounted
> by both vCenters, ownership handoff on that datastore, intra-VC relocate to
> the final target datastore — built entirely from supported primitives.

## 1. Problem Statement

The vcf-migration-operator migrates an OpenShift cluster from a source vCenter
(vSphere 8.x) to a target vCenter (VCF/VVF 9) by rolling new machines through
Machine API / CPMS. Compute moves; storage does not.

PersistentVolumes provisioned by the vSphere CSI driver (`csi.vsphere.vmware.com`)
are CNS volumes backed by First Class Disks (FCDs). Their identity — the PV
`spec.csi.volumeHandle` (FCD UUID) — lives in the **source vCenter's** vslm/CNS
catalogs. The commonly known `CnsRelocateVolume` API historically relocated
volumes only between datastores of the *same* vCenter/datacenter. Consequently,
Phase 1 hard-fails preflight when any vSphere CSI-backed PV exists.

This document designs Phase 2: migrating CNS volumes (data + CNS registration +
Kubernetes PV objects) from the source vCenter to the target vCenter, and
enumerates all alternatives considered.

## 2. Key API Finding: Cross-vCenter `CnsRelocateVolume` (vSphere 8.0U3+)

`CnsVolumeRelocateSpec` carries an optional `ServiceLocator` that points at a
*remote* vCenter, making `CnsRelocateVolume` a **cross-vCenter** migration API:

```go
// github.com/vmware/govmomi/cns/types
type CnsVolumeRelocateSpec struct {
    types.DynamicData
    VolumeId       CnsVolumeId                           `xml:"volumeId"`
    Datastore      types.ManagedObjectReference          `xml:"datastore"`      // MoRef from the TARGET vCenter
    Profile        []types.BaseVirtualMachineProfileSpec `xml:"profile,omitempty,typeattr"`
    ServiceLocator *types.ServiceLocator                 `xml:"serviceLocator,omitempty"` // remote (target) vCenter
}
```

- Supported from **vSphere 8.0U3** onward (explicitly version-gated in govmomi's
  `cns/client_test.go`: `isvSphereVersion80U3orAbove`, exercised via
  `REMOTE_VC_URL` / `REMOTE_DATACENTER`).
- govmomi ships a production helper `cns.GetServiceLocatorInstance(ctx, user,
  password, remoteVCClient)` that builds the `ServiceLocator` (instance UUID,
  SDK URL, credentials, SSL thumbprint).
- The call is issued against the **source** vCenter's CNS endpoint; CNS
  orchestrates the data transfer to the target datastore and the catalog
  re-registration on the target vCenter.
- The volume **must be detached** (no VM attachment, i.e., no running pod using
  it) at relocation time.
- vSphere 9.1 additionally introduces `CnsUnregisterVolume` (CNS volume → bare
  FCD), useful for fallback flows; vSphere 9.x otherwise builds on the 8.0U3
  cross-VC primitive rather than replacing it.

Our source (vSphere 8.x) → target (VCF/VVF 9) scenario satisfies the version
requirement as long as the source is at 8.0U3 or later. **Preflight must gate on
source vCenter ≥ 8.0U3** when CSI volumes are present.

> **Support status (2026-08-13)**: although the API surface exists and our
> spike proved it functional in specific configurations (§8), Broadcom support
> has stated that direct cross-vCenter datastore-to-datastore CNS relocation
> is **not supported** at present (§8.1). The MoRef validation defect we
> reported (§8, finding 2) makes it unreliable in practice anyway. This API
> remains relevant as (a) a fast path if/when Broadcom supports it, and (b)
> the supported **intra-VC** relocation primitive used by the staged relay.

## 3. Alternatives Considered

| # | Approach | APIs | Supportability | FCD UUID preserved | CNS metadata | Verdict |
|---|----------|------|----------------|--------------------|--------------|---------|
| A | **Cross-VC `CnsRelocateVolume` + `ServiceLocator`** | `cns.Client.RelocateVolume`, `cns.GetServiceLocatorInstance` | ❌ **Declared unsupported by Broadcom** (2026-08-13, support case; see §8.1). Functionally works in the lab but gated by a MoRef-ID-coincidence defect (§8 finding 2) | ✅ validated in spike | ✅ CNS orchestrates transfer & registration | ~~Selected~~ → **opportunistic fast path only** (feature-flagged, off by default) |
| B | Unregister → copy VMDK → `RegisterDisk` on target → `CnsCreateVolume(BackingDiskId)` | `CnsUnregisterVolume` (9.1+), `vslm.RegisterDisk`, `CnsCreateVolume` | ⚠️ Partial: unregister needs 9.1; the data-copy step is out-of-band/unofficial | ❌ new UUID | ❌ must recreate | Fallback only |
| C | Shared datastore re-registration (no data copy): datastore visible to both vCenters (NFS / vSAN HCI Mesh); `ReconcileDatastoreInventory` + `CnsCreateVolume(BackingDiskId)` on target | `vslm.ReconcileDatastoreInventory_Task`, `vslm.RegisterDisk`, `CnsCreateVolume` | ✅ Uses only supported intra-VC primitives | ✅ likely (FCD metadata travels with datastore catalog; needs spike validation) | ⚠️ recreated via `CnsCreateVolume(BackingDiskId)` + `CnsUpdateVolumeMetadata` | **Now the ownership-handoff step of the selected staged relay (§4.3a)** |
| A+C | **Staged shared-datastore relay**: intra-VC relocate → shared DS → C-style handoff → intra-VC relocate to final DS | intra-VC `CnsRelocateVolume` ×2 + C's re-registration APIs | ✅ every step individually supported | ✅ (intra-VC relocate preserves ID; handoff preserves ID) | ⚠️ recreated at handoff | **Selected (revised design basis)** |
| D | Raw cross-vCenter vMotion of node VMs carrying attached FCDs ("shared-nothing" XvMotion) | `VirtualMachine.Relocate` w/ ServiceLocator | ❌ Not supported for attached CNS volumes — CNS/CSI metadata orphaned on both sides | possible via reconcile | ❌ broken | Rejected |
| E | CNS snapshot + `CloneVStorageObject` to target | `CnsCreateSnapshots`, `vslm.Clone` | ❌ vslm Clone/Relocate are intra-vCenter only (no ServiceLocator in vslm specs) | ❌ | ❌ | Rejected |
| F | HCX-driven migration | HCX + A/B | ⚠️ HCX explicitly does not migrate CSI-managed PVs; solves only compute | n/a | ❌ | Rejected as storage solution (compatible with A for VMs, but this operator doesn't vMotion VMs anyway) |
| G | Backup/restore (OADP/Velero/MTC) into a re-provisioned target StorageClass | Velero + CSI snapshots or file copy | ✅ Supported products | ❌ new volumes | new volumes | Rejected as primary: heavy downtime, app-level involvement, exactly the "rebuild + MTC" experience this operator exists to avoid. Remains the user fallback for unsupported volume types |
| H | Application-level replication (e.g., operators with built-in replication, rsync pods) | n/a | app-specific | ❌ | ❌ | Out of scope; user-managed |

The revised design basis is **A+C, the staged shared-datastore relay**, with
the direct cross-VC relocate (A) kept behind a feature flag as an opportunistic
fast path, **B** as a fallback, and **G** as the escape hatch for anything
Phase 2 cannot migrate (see §10 Limits).

### Why A+C over the others (revised 2026-08-13)

- Broadcom declared direct cross-VC relocate (A alone) unsupported, and our
  spike found a MoRef-validation defect that makes it unusable in general
  environments regardless (§8 finding 2).
- Every step of the relay is an individually supported operation: intra-VC
  `CnsRelocateVolume` (GA since CSI relocation was introduced), vslm datastore
  catalog reconciliation / disk registration, and `CnsCreateVolume` with
  `BackingDiskId` (the standard "static provisioning" path the CSI driver
  itself uses).
- The FCD UUID (and therefore the PV `volumeHandle`) is preserved: intra-VC
  relocation preserves it (validated, §8), and re-registration adopts the
  existing FCD rather than creating a new one.
- Prerequisite cost: one datastore (e.g., an NFS export) temporarily mounted
  by ESXi hosts in **both** vCenters. This is a real constraint (see §4.3a
  "Staging datastore requirements") but far weaker than HCI Mesh or Enhanced
  Linked Mode, is temporary, and needs capacity only for the in-flight batch,
  not the whole dataset.

## 4. Design Overview

### 4.1 Where storage migration fits in the phase pipeline

Ordering constraint: a volume can only be *attached* by the vCenter that owns
it. Pods on source-vCenter nodes need volumes on the source vCenter; pods on
target-vCenter nodes need volumes on the target vCenter. Volumes must be
detached to relocate. Therefore volume migration is interleaved with workload
migration, per-workload:

```
InfrastructurePrepared
DestinationInitialized
MultiSiteConfigured          ← cluster + CSI driver see both vCenters
StorageMigrated (NEW)        ← per-PV: quiesce → detach → CnsRelocateVolume → PV rewrite → resume
WorkloadMigrated             ← existing: CPMS + MachineSets roll compute to target
SourceCleaned
Ready
```

Refinement: target worker MachineSets are created at the *start* of the
`StorageMigrated` phase (pulled forward from `WorkloadMigrated`), so that when a
workload's volumes land on the target vCenter its pods can immediately
reschedule onto target nodes. The phase loop is:

1. Ensure at least the target worker capacity needed for stateful workloads
   exists (reuse existing MachineSet creation logic from `WorkloadMigrated`).
2. Migrate volumes workload-by-workload (see 4.3).
3. `WorkloadMigrated` then completes as today (CPMS rollout, remaining workers,
   scale source sets to zero).

### 4.2 CSI driver multi-vCenter posture

During `MultiSiteConfigured` the operator already merges the target vCenter into
`Infrastructure` and `cloud-provider-config`. For Phase 2 it must also ensure
the vSphere CSI driver (managed by `vmware-vsphere-csi-driver-operator` via
cluster-storage-operator) is running with **multi-vCenter topology**
(`multi-vcenter-csi-topology`, CSI v3.x) so that:

- volumes still on the source vCenter remain attach/detach-able for pods on
  source nodes,
- migrated volumes are attach-able for pods on target nodes,
- the driver's `CNSVolumeInfo` CRs map each `volumeID → vCenter` during the
  transitional state.

Preflight addition: verify the OpenShift version's CSI operator supports
multi-vCenter (OCP 4.17+ multi-vCenter support; exact floor to be pinned) —
otherwise fail before pausing CVO, exactly as Phase 1 does today.

### 4.3 Per-volume migration workflow

Steps 1–2 and 4–7 below are common; step 3 (the actual move) has two
implementations: the **staged relay** (§4.3a, primary) and the **direct
cross-VC relocate** (§4.3b, feature-flagged fast path).

Common workflow, for each vSphere-CSI PV, grouped by consuming workload (see 4.4):

```
1. Quiesce      Scale down / evict pods using the PVC(s).
                (annotation-driven opt-out lets users quiesce themselves)
2. Verify detach  Wait for VolumeAttachment deletion; confirm via
                CnsQueryVolume that the volume has no attached VM.
3. Move         Staged relay (§4.3a, default) or direct cross-VC relocate
                (§4.3b, feature-flagged). Both preserve the volume ID.
4. Rewrite PV   If the volume ID changed (defensive; not observed in spike):
                recreate the PV object with the new volumeHandle while
                preserving claimRef (delete-with-Retain + recreate pattern,
                same technique as static provisioning), update datastore URL,
                topology keys (region/zone labels + nodeAffinity) to the
                target failure domain.
                If ID is preserved: patch only topology/labels as needed.
5. Metadata     Ensure CNS entity metadata on target (PV/PVC/pod names,
                cluster ID) via CnsUpdateVolumeMetadata; verify CNSVolumeInfo
                CR now maps the volume to the target vCenter.
6. Resume       Un-quiesce. Scheduler places pods on target-vCenter nodes
                (source nodes lack the volume's new topology / cannot attach).
7. Record       Per-volume status entry: Succeeded / Failed + task info.
```

Volumes not attached to any pod (released/idle PVCs) are migrated first, with
no quiesce step — they are the cheap majority in many clusters and validate the
pipeline early.

### 4.3a Staged shared-datastore relay (primary path)

Prerequisite: a **staging datastore** mounted (temporarily) by ESXi hosts in
both vCenters — typically an NFS export; any datastore type both host fleets
can mount works. Configured as `spec.storage.stagingDatastore`.

```
S1. Stage-in     SOURCE CNS: intra-VC CnsRelocateVolume
                 (volumeId, datastore=<staging DS moref on SOURCE VC>,
                 no ServiceLocator). Data copies source DS → staging DS.
                 Volume ID preserved (validated, §8).
S2. Release      SOURCE VC: remove the CNS registration while leaving the FCD
                 files on the staging datastore:
                   CnsDeleteVolume(volumeId, deleteDisk=false)
                 (validated §8.2; works on 8.x and 9.x. Note
                 CnsUnregisterVolume, though present in 9.1, is not reachable
                 through govmomi's negotiated vSAN WSDL version.)
S3. Adopt        TARGET VC: the FCD catalog lives on the datastore itself, so
                 the target vslm often already lists the FCD. Otherwise call
                 vslm.ReconcileDatastoreInventory_Task(staging DS) and POLL
                 Retrieve() — the reconcile task completes before the FCD is
                 retrievable. UUID intact either way.
S4. Register     TARGET CNS: CnsCreateVolume(BackingDiskId=<FCD UUID>) with
                 the cluster's Kubernetes metadata (PV/PVC names, cluster
                 ID) and NO Datastores field — the same static-provisioning
                 path the CSI driver uses.
S5. Stage-out    TARGET CNS: intra-VC CnsRelocateVolume
                 (volumeId, datastore=<final target DS moref on TARGET VC>,
                 Profile=<target SPBM policy>). Data copies staging DS →
                 final DS (e.g., target vSAN). Volume ID preserved.
```

Properties:

- Every call is an intra-vCenter operation of the vCenter it runs on — no
  cross-VC API involved, hence fully within Broadcom support. **Validated
  end to end on 9.1→9.1 (§8.2), including the configuration where the direct
  cross-VC call is refused server-side.**
- Volume ID (FCD UUID / PV `volumeHandle`) preserved at every step.
- No dependency on the ServiceLocator DNS requirement or the MoRef-ID
  coincidence defect (§8 findings 2–3): each moref is resolved by the vCenter
  that owns it.
- Failure at any step leaves the volume in a well-defined, recoverable state:
  S1 fail → volume intact on source DS; S2–S4 fail → FCD intact on staging DS
  (re-register on whichever side to retry/roll back); S5 fail → volume
  registered on target, still on staging DS (retry relocate later).
- Cost: two data copies per volume (source→staging, staging→final). The
  staging datastore needs capacity for `maxConcurrentMigrations` × largest
  volume batch only; volumes are pipelined through and it is empty afterward.
- Rollback of a migrated volume is the mirror sequence.

Staging datastore requirements (preflight-validated):

- Mounted read-write by at least one host in every relevant cluster of both
  vCenters (used for relocate placement).
- Not vSAN (vSAN datastores cannot be cross-mounted between unrelated
  clusters without HCI Mesh); NFS v3/v4.1 or VMFS on shared SAN LUNs both
  qualify.
- Free capacity ≥ the largest concurrent migration batch (+ slack).
- May be created just for the migration and unmounted afterward.

### 4.3b Direct cross-VC relocate (feature-flagged fast path)

The single-call variant validated in the spike (§8): source CNS
`CnsRelocateVolume` with `ServiceLocator` pointing at the target vCenter.
Kept behind `spec.storage.allowDirectRelocate: true` (default false) because
Broadcom currently declares it unsupported (§8.1), and it only works when the
target datastore's MoRef ID coincidentally exists on the source vCenter.
If Broadcom later supports it (our defect report may drive this), flipping
the default turns the two-copy relay into a single-copy direct move with no
staging datastore requirement.

```
D1. sl := cns.GetServiceLocatorInstance(ctx, targetUser, targetPass, targetVC)
D2. sourceCNS.RelocateVolume(CnsBlockVolumeRelocateSpec{
        VolumeId, Datastore: <target DS moref>, Profile, ServiceLocator: sl})
D3. Poll CNS task; then poll TARGET CnsQueryVolume until the async catalog
    sync registers the volume (§8 finding 5).
```

### 4.4 Batching and orchestration unit

- Unit of quiesce = workload (all PVCs mounted by the same pod controller move
  together, since a pod cannot mount volumes from two vCenters if scheduling
  constraints diverge).
- `spec.storage.maxConcurrentMigrations` (default 2) bounds parallel CNS
  relocate tasks — cross-VC relocation is a full data copy over the network and
  can be long-running; CNS task concurrency limits on vCenter apply.
- Idempotency: before relocating, query target CNS (`CnsQueryVolume` by volume
  ID / by Kubernetes metadata filter) to detect an already-migrated volume and
  skip to step 4 — this makes retries and operator restarts safe.

### 4.5 API changes (CRD)

```yaml
spec:
  storage:
    # Storage migration mode:
    #   Automatic  – operator quiesces workloads and migrates volumes (default)
    #   Manual     – operator migrates only PVCs annotated ready-to-migrate;
    #                user controls quiesce timing
    #   Skip       – preserve Phase 1 behavior: fail preflight if CSI PVs exist
    mode: Automatic
    maxConcurrentMigrations: 2
    # Staging datastore mounted by hosts in BOTH vCenters (required for the
    # primary staged-relay path; must not be vSAN). Name as seen by each VC.
    stagingDatastore: "migration-staging-nfs"
    # Opt-in to the single-call direct cross-VC relocate (currently
    # unsupported by Broadcom; see design doc §8.1). Default false.
    allowDirectRelocate: false
    # Optional per-failure-domain override of the destination datastore for
    # volumes; defaults to failureDomains[].topology.datastore.
    # Optional target SPBM storage policy name; defaults to the datastore
    # default policy.
    targetStoragePolicy: ""
status:
  storage:
    total: 12
    migrated: 7
    failed: 0
    volumes:              # capped/paginated; details also emitted as Events
      - pvName: pvc-abc…
        sourceVolumeID: …
        targetVolumeID: …
        state: Succeeded | InProgress | Pending | Failed
        message: ""
```

New condition `StorageMigrated`, ordered between `MultiSiteConfigured` and
`WorkloadMigrated`. When `mode: Skip` (or zero CSI PVs), the condition is set
`True` with reason `Skipped`/`NoVolumes`.

Alternative considered for status: a separate namespaced CR per volume
(`VolumeMigration`). Rejected for v1 — the singleton CR's status list plus
Events is simpler and matches the existing operator style; revisit if status
size becomes a problem (>100s of PVs → move to a ConfigMap/CR-per-volume).

### 4.6 Preflight changes (replaces the hard failure)

When vSphere CSI PVs are detected and `mode != Skip`:

1. Source vCenter version ≥ 8.0U3 (`ServiceContent.About`), else fail with a
   message pointing at the requirement.
2. Target vCenter reachable and `GetServiceLocatorInstance` constructible
   (thumbprint retrievable).
3. `spec.storage.stagingDatastore` exists on **both** vCenters, is not vSAN,
   is mounted read-write by hosts in the relevant clusters on both sides, and
   has free capacity ≥ the largest migration batch. (Skipped when
   `allowDirectRelocate: true` and the direct path's own preconditions hold —
   including target-datastore MoRef existence on the source and ServiceLocator
   hostname resolvability from the source VCSA, §8 findings 2–3.)
4. OpenShift release supports multi-vCenter CSI topology.
5. Every CSI PV's datastore/policy maps to a valid target datastore + SPBM
   policy in some failure domain.
6. Enumerate **unsupported volume types** and fail early (see §10): RWX/file
   volumes (vSAN File Services), volumes with CNS snapshots (unless user
   accepts snapshot loss — gate behind an explicit acknowledgement field),
   encrypted volumes (until validated), in-tree `vsphereVolume` PVs that were
   never migrated to CSI.
6. Capacity check: target datastore free space ≥ sum of volume sizes (+ slack).

### 4.7 New internal package

`internal/vsphere/cns`:

- `NewCNSClient(session)` — wraps `github.com/vmware/govmomi/cns` (add to
  go.mod/vendor; vslm too).
- **Staged relay (primary)**: `RelocateVolumeIntraVC(ctx, volID, targetDS,
  policy)` (used on both source and target for stage-in/stage-out),
  `ReleaseVolume` (CnsUnregisterVolume on 9.1+ / delete-keep-disk on 8.0U3),
  `AdoptFCD` (`vslm.ReconcileDatastoreInventory_Task` / `RegisterDisk`),
  `RegisterExistingFCD` (`CnsCreateVolume` with `BackingDiskId` + K8s
  metadata).
- **Direct path (feature-flagged)**: `RelocateVolumeCrossVC(ctx, volID,
  targetDS, policy, serviceLocator)` with CNS task polling and typed results.
- `QueryVolumeByID` (with async-registration retry, §8 finding 5),
  `QueryVolumesForCluster` (filter by cluster ID), `UpdateVolumeMetadata`.

`internal/openshift/storage`:

- PV/PVC/VolumeAttachment inventory and classification (CSI vs in-tree vs
  other drivers).
- Workload discovery (PVC → pods → owning controller) and quiesce/resume
  (scale to zero with original replica count stashed in an annotation; respect
  PodDisruptionBudget by using eviction where scaling isn't possible, e.g.
  static pods are out of scope).
- PV rewrite: Retain-policy flip → delete PV object (not data) → recreate with
  new `volumeHandle`/topology → verify Bound.

## 5. Failure Handling & Rollback

- Staged relay: each step leaves the volume in a well-defined state (see
  §4.3a); the FCD is never simultaneously unregistered *and* absent. The
  operator records the per-volume relay step in status so retries resume from
  the right step after operator restart.
- Direct path: each relocate is atomic at the CNS level: task failure leaves
  the volume on the source vCenter, still registered; the operator resumes
  the workload in place and marks the volume `Failed` (migration overall
  pauses after N consecutive failures, condition
  `StorageMigrated=False/Failed`).
- Partial-phase pause (`spec.state: Paused`) is safe at any point: cluster runs
  in multi-vCenter mode with volumes split across vCenters; multi-VC CSI keeps
  both sides functional.
- Rollback of a migrated volume = run the relay in the opposite direction.
  Not automated in v1; documented as a manual govc/CLI procedure.
- `SourceCleaned` gains a gate: refuse to remove the source vCenter while any
  CSI volume still resides there (CnsQueryVolume on source for this cluster ID
  must return zero volumes).

## 6. Security

- Target vCenter credentials are already in the migration Secret; the
  `ServiceLocator` embeds them and they transit to the source vCenter over TLS
  — document this trust implication (source VC learns target VC credentials
  for the duration of the call). Recommend a scoped migration role on the
  target vCenter (CNS + datastore + vslm privileges only).
- SSL thumbprint of the target is fetched and pinned in the ServiceLocator.

## 7. Testing Strategy

- Unit: `internal/vsphere/cns` against govmomi `simulator` (vcsim supports CNS
  API surface partially; extend/stub where missing — vcsim has no cross-VC
  relocate, so simulate at the client-wrapper boundary).
- envtest: reconciler ordering, condition transitions, PV rewrite logic,
  quiesce/resume with fake workloads.
- e2e (real hardware, dual vCenter lab, source ≥ 8.0U3): the definitive test —
  validates the open questions below (UUID preservation, throughput, task
  concurrency, attached-volume error behavior). This must run before design
  finalization ("spike" milestone).

## 8. Spike Results (2026-08-10, nested lab: vCenter 8.0.3/ESXi 8.0.3 → vCenter 9.1.0/ESXi 9.0.0)

Validated with `hack/cns-migrate` (govmomi v0.52.0):

| Test | Result |
|------|--------|
| Intra-VC relocate (vSAN → NFS, source VC) | ✅ ~1s, volume ID preserved |
| Cross-VC relocate, source volume on shared NFS datastore → target vSAN | ✅ 8–19s, **volume ID preserved**, CNS + FCD catalog ownership fully transferred (source CNS empty, target CNS/vslm populated) |
| Cross-VC relocate, source volume on source-only vSAN → target vSAN | ✅ 3–4s once the source VCSA could resolve the target vCenter hostname (see finding 3), volume ID preserved. Earlier failures (`SystemError: vim.fault.NotSupportedHost`) were caused by DNS resolution failure on the source VCSA; CNS rolled back cleanly each time |
| Cross-VC relocate, target datastore whose MoRef ID does not exist in the *source* VC inventory | ❌ immediate `InvalidArgument: VolumeRelocateSpec.datastore` |
| Two-hop workaround: intra-VC relocate onto a datastore mounted by both vCenters, then cross-VC relocate to final target | ✅ works end to end, ID preserved (not needed once DNS was fixed — direct vSAN→vSAN works) |

Findings and implications:

1. **Volume ID (FCD UUID / PV volumeHandle) is preserved** across vCenters —
   PV objects may only need topology/labels updates, not volumeHandle rewrite.
   (Open question 1 answered for this path.)
2. **MoRef validation quirk (confirmed by experiment)**: the source vCenter
   pre-validates `VolumeRelocateSpec.datastore` against **its own inventory by
   numeric ID only**, then the ID is resolved on the **target** vCenter for
   the actual placement. Proven: relocating to target `datastore-21`
   (`219-…esx-install`) succeeded *only because* the source also has a
   `datastore-21` (a completely different datastore, `150-…esx-install`); the
   FCD landed on the target's datastore-21. Relocating to target `nfs2`
   (`datastore-38`, no such ID on source) fails instantly with
   `InvalidArgument: VolumeRelocateSpec.datastore` even with working DNS.
   Consequences: (a) a target datastore is reachable only if a datastore with
   the *same numeric moref ID* happens to exist on the source — pure
   coincidence in real environments; (b) when the operator cannot use direct
   relocate because of this, it must fall back to the two-hop staging
   pipeline; (c) this is almost certainly a vpxd pre-validation bug — should
   be filed with VMware/Broadcom.
3. **`NotSupportedHost` is a red herring for "cannot reach remote vCenter"**.
   Root cause (from source VCSA `vpxd.log`): vpxd on the *source* vCenter must
   itself resolve and connect to the ServiceLocator URL
   (`Failed to resolve address; '<target-vc>:443' … Host not found` →
   `Not able to establish remote connection` → surfaced as
   `vim.fault.NotSupportedHost` wrapped in `SystemError` wrapped in
   `CnsFault: VSLM task failed`). Adding the target VC to the source VCSA's
   DNS (/etc/hosts in the lab) made direct vSAN→vSAN relocation succeed in
   ~3s. **Operator implication**: preflight must verify the ServiceLocator URL
   hostname is resolvable/reachable *from the source vCenter*, not just from
   the cluster; consider using an IP or FQDN known to resolve on the VCSA.
4. **Shared-datastore staging works reliably** and remains a fallback: when
   the volume resides on a datastore mounted by target hosts, cross-VC
   relocate is fast (ownership handoff, no data copy) — two-hop pipeline:
   `source DS → (intra-VC) staging DS → (cross-VC) target DS`.
5. **Target-side CNS registration is asynchronous**: immediately after a
   successful relocate the FCD is on the target datastore (`govc disk.ls`)
   but `CnsQueryVolume` on the target may return NOT FOUND for a minute or
   more before the entry appears (with `health: unknown` initially). The
   operator must poll/retry target CNS queries rather than treat NOT FOUND as
   failure; the vslm FCD catalog is authoritative.
6. Post-migration CNS `datastoreUrl` on the target can be momentarily stale.
7. Rollback direction (target→source relocate) still needs testing, as do:
   attached-volume error surface, snapshots, throughput at real volume sizes.

### 8.1 Broadcom support determination (2026-08-13)

We filed a support case with Broadcom covering the spike findings (the MoRef
validation defect and the misleading `NotSupportedHost` fault; full report in
`broadcom-cns-relocate-bug-report.md`). Broadcom's response: **direct
datastore-to-datastore migration of CNS volumes across vCenters is
unsupported**, at least currently.

**Empirical confirmation (2026-08-13, 9.1→9.1 lab)**: with a vCenter 9.1.0
*source* (two independent 9.1.0 vCenters, DNS resolvable, identical
`datastore-19` vSAN morefs on both — the exact configuration that succeeded
8.0.3→9.1), `CnsRelocateVolume` with a ServiceLocator is rejected
synchronously with a purpose-built fault:

```
CnsFault error: Cross VC volume migration is not supported!
```

So vCenter 9.x **actively blocks** the cross-VC path that 8.0U3–8.0.3 exposed.
Implications:

- 8.0.3 source → 9.1 target: works functionally (our spike) but vendor
  unsupported and dependent on the MoRef-ID coincidence defect.
- 9.x source → any target (e.g., a customer already on VCF 9 moving between
  VCF instances, or *rollback* of a volume already migrated to a 9.1 target):
  **impossible via direct relocate — guard is enforced server-side.**
  In particular, rolling a migrated volume back from the 9.1 target requires
  the staged relay; the direct path cannot do it even where it worked forward.

Design consequences (applied throughout this revision):

- Approach A alone cannot be the design basis for a supported product
  feature, regardless of the fact that it works in favorable configurations.
- The **staged shared-datastore relay** (§4.3a) becomes the primary path: it
  uses only intra-vCenter relocation plus the standard FCD/CNS registration
  APIs, all individually supported.
- The direct cross-VC call is retained behind an off-by-default flag
  (§4.3b): if Broadcom later supports it (potentially motivated by our defect
  report), it removes the staging-datastore prerequisite and halves the data
  movement.
- The staged relay's S2–S4 handoff needs its own spike (M2) to pin the exact
  minimal release/adopt/register sequence on an 8.0U3 source, since
  `CnsUnregisterVolume` only exists from 9.1. ✅ **Done — see §8.2**; the
  sequence is version-portable (`CnsDeleteVolume(deleteDisk=false)`).

### 8.2 Staged relay validated end-to-end (2026-08-13, VCF 9.1 → VCF 9.1 lab)

Second lab: two independent vCenter 9.1.0 (build 24840651) instances
(`vcf9` → `vcf92`), both mounting the **same** NFS41 export
(`…:/nfsmount2/devqe2`, datastore `nfs2`, identical volume UUID) plus their
own local vSAN. Implemented as `cns-migrate relay` in `hack/cns-migrate`.

| Test | Result |
|------|--------|
| Direct cross-VC `CnsRelocateVolume` (9.1 source) | ❌ `CnsFault error: Cross VC volume migration is not supported!` — rejected synchronously (§8.1) |
| **Full relay S1→S5, single volume** (vSAN → nfs2 → adopt/register → target vSAN) | ✅ **volume ID preserved**, target CNS `health: green`, source CNS entry gone |
| **Relay, 3 idle volumes in parallel** | ✅ 3/3 in **24 s total** (S1 2 s each, S5 21–23 s each), no interference, all IDs preserved |
| Relay resume after a mid-way failure | ✅ tool detects "not registered in source CNS" and resumes at S3 |

Step-by-step findings (these are implementation requirements for the
operator's `internal/vsphere/cns` package):

1. **S1 stage-in** (intra-VC relocate to the shared datastore): works, 2 s for
   a 2 GiB volume, ID preserved. Re-running when the volume is already on the
   staging datastore fails with `already exists` — the operator must check the
   volume's current datastore and skip (idempotency).
2. **S2 release**: `CnsUnregisterVolume` is **not usable via govmomi v0.52.0**
   even against 9.1 — the negotiated vSAN WSDL namespace rejects it
   (`Unable to resolve WSDL method name CnsUnregisterVolume in
   vsan.version.vsandev`). The working, version-portable primitive is
   **`CnsDeleteVolume(volumeId, deleteDisk=false)`**, which removes the CNS
   registration and leaves the FCD on the datastore. This is also the only
   option on an 8.0U3 source, so the operator should standardise on it.
3. **S3 adopt on target**: because the FCD catalog lives on the datastore, the
   target vCenter frequently **already has the FCD in its vslm catalog** — no
   action needed. When it does not, `ReconcileDatastoreInventory_Task` works
   but is **asynchronous**: the task reports completion before the FCD is
   retrievable. The operator must poll `Retrieve` after reconcile (we poll for
   up to 5 min; in the lab the FCD surfaced within ~1 min).
4. **S4 register on target**: `CnsCreateVolume` with
   `BackingObjectDetails.BackingDiskId = <FCD UUID>` and **no `Datastores`
   field** — supplying `Datastores` fails with
   `InvalidArgument: createSpecs.datastores`. Volume ID is preserved; CNS
   metadata (cluster ID, distribution) is supplied fresh in this call, so
   there is no metadata carry-over problem to solve.
5. **S5 stage-out** (intra-VC relocate on target to the final datastore):
   works, ID preserved. Timing varied widely — 2m42s for the first (cold)
   volume vs 21–23 s each for three parallel volumes, suggesting first-touch
   NFS read / vSAN object warm-up rather than a fixed per-volume cost.
6. **Transient dual registration is possible and self-heals**: while the FCD
   sits on the shared staging datastore, the **source** vCenter's `CnsSync`
   can re-adopt it into its own CNS database (observed: the source re-created
   an entry after S2). Once the volume is relocated off the staging datastore
   in S5, the source entry disappears on the next sync. Operator implication:
   do not treat a lingering source CNS entry during the staging window as a
   failure, but **do** verify the source is clean after S5 before
   `SourceCleaned` (§5).
7. **Parallelism is safe for idle (detached) volumes**: three concurrent
   relays showed no lock contention, no task serialisation and no cross-talk;
   vCenter scheduled the CNS tasks independently on both sides. This supports
   a `maxConcurrentMigrations` default > 1, bounded by datastore/network
   throughput rather than API limits.
8. The whole relay uses **only intra-vCenter calls** — validated on the very
   configuration where the direct cross-VC call is refused by the server.

## 9. Remaining Open Questions

1. **Volume ID preservation**: ✅ answered — preserved in all successful spike
   runs (see §8). PV rewrite logic downgraded to topology patching, with the
   recreate path kept as a fallback.
2. **Staged relay handoff (S2–S4)**: ✅ answered — validated end to end on
   9.1→9.1 (§8.2). Release with `CnsDeleteVolume(deleteDisk=false)`, adopt via
   the datastore-resident FCD catalog (`ReconcileDatastoreInventory` only when
   needed, with polling), register with `CnsCreateVolume(BackingDiskId)` and
   **no** `Datastores` field. UUID preserved throughout. Still to confirm on
   an **8.0U3 source** (the primary customer scenario).
3. ✅ answered — S4 supplies Kubernetes entity metadata directly in
   `CnsCreateVolume`, so there is no carry-over gap; `CnsUpdateVolumeMetadata`
   is only needed for later PV/PVC/pod name updates.
4. Behavior with SPBM: is `Profile` mandatory when the final target is vSAN
   (VCF 9 default storage)?
5. CNS snapshots: relocate is expected to fail for volumes with snapshots —
   confirm, and confirm error surface.
6. Throughput and vCenter-side concurrency limits for intra-VC relocation at
   scale; ✅ partially answered — 3 concurrent relays ran cleanly with no API
   contention (§8.2 finding 7); still need numbers at realistic volume sizes
   and higher concurrency.
7. Interaction with the OpenShift vSphere CSI *operator*: will it fight our
   driver configuration during the transitional multi-vCenter window (CVO is
   paused, which helps, but cluster-storage-operator still runs)?
8. Minimum OCP version providing multi-vCenter CSI topology support.
9. Whether volumes on vVOL datastores are supported by intra-VC relocate +
   the relay handoff.
10. Will Broadcom support direct cross-VC relocate in a future release (our
    case may drive a roadmap answer)? Track the case; revisit §4.3b default.

## 10. Explicit Limits (v1 of Phase 2)

- Block RWO CNS volumes only. **Not** migrated: vSAN File Services RWX
  volumes, in-tree `vsphereVolume` PVs (must be CSI-migrated first), volumes
  with CNS snapshots (snapshots are not transferable; user must delete or
  accept loss), guest/supervisor (WCP) clusters.
- **A staging datastore mountable by both vCenters' hosts is required** for
  the supported path (until/unless Broadcom supports direct cross-VC
  relocation). Environments that cannot present any shared NFS/VMFS datastore
  to both host fleets cannot use Phase 2 automation and fall back to G.
- Downtime per stateful workload equals detach + data-copy ×2 + reattach time;
  no live migration exists for CNS volumes. `mode: Manual` gives users control
  of the window.
- No automated rollback of already-migrated volumes.
- Anything excluded above falls back to alternative G (OADP/MTC) and is called
  out in preflight messaging.

## 11. Rollout Plan

1. **Spike** (lab): validate approach A end-to-end with govc/standalone Go
   program; answer §8. ✅ Done — outcome: A works but is vendor-unsupported
   (§8.1); design rebased on the staged relay.
2. **M1**: preflight replacement (version gates, classification, capacity,
   staging-datastore validation), `spec.storage` API, `Skip` mode default
   (keeps GA behavior unchanged).
3. **M2**: staged-relay spike (pin the S2–S4 handoff sequence on 8.0U3
   source; open question 2) + `internal/vsphere/cns` + idle-volume migration
   (no quiesce path).
4. **M3**: workload quiesce/resume + full `Automatic` mode; `StorageMigrated`
   condition wired into the pipeline; `SourceCleaned` gate.
5. **M4**: `Manual` mode, `allowDirectRelocate` fast path, console plugin
   surfacing per-volume progress, docs.
6. Enhancement update: amend openshift/enhancements#1961 (or file the Phase 2
   enhancement) removing the "fail on CSI PVs" non-goal.
