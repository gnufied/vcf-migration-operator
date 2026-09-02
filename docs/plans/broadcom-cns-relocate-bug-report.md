# Bug Report: Cross-vCenter `CnsRelocateVolume` validates target datastore MoRef against the SOURCE vCenter inventory

Prepared for: Broadcom / VMware by Red Hat OpenShift engineering
Date: 2026-08-11
Severity: High — makes cross-vCenter CNS volume relocation succeed or fail
based on numeric MoRef-ID coincidence between two unrelated vCenters.

## Summary

When calling `CnsRelocateVolume` with a `CnsBlockVolumeRelocateSpec` whose
`serviceLocator` points at a **remote** (target) vCenter, the **source**
vCenter's CNS service (`vsanvcmgmtd`) resolves and authorizes
`VolumeRelocateSpec.datastore` — a MoRef that semantically belongs to the
**target** vCenter — against the **source** vCenter's own inventory.

Consequences:

1. If no datastore with the same numeric ID (e.g. `datastore-38`) exists on
   the source vCenter, the call fails immediately with
   `InvalidArgument: A specified parameter was not correct: VolumeRelocateSpec.datastore`,
   even though the datastore exists on the target vCenter.
2. If a datastore with the same numeric ID exists on the source — even a
   **completely different, unrelated datastore** — validation and the
   `CnsCatalogSvc` privilege check pass against that wrong source-side object,
   and the operation then proceeds and places the disk on whatever that ID
   resolves to on the **target** vCenter.

In other words, whether a cross-vCenter relocation is possible at all is
determined by an accidental collision of MoRef ID counters between two
independent vCenters, and the privilege check is evaluated against the wrong
object. We could find no supported way to relocate a CNS volume to a target
datastore whose MoRef ID does not happen to exist on the source vCenter.

A second, related issue (documented in Appendix B): a failure of the source
vCenter to DNS-resolve the `serviceLocator.url` hostname is surfaced as
`vim.fault.NotSupportedHost` (wrapped in `SystemError` / `CnsFault: "VSLM
task failed"`), which is highly misleading and cost significant debugging
time.

## Environment

| | Source | Target |
|---|---|---|
| vCenter | 8.0.3, build 24022515 | 9.1.0.0, build 24840651 |
| ESXi hosts | 3× ESXi 8.0.3 (nested) | 3× ESXi 9.0.0 (nested) |
| Primary datastore | vSAN (`vsanDatastore`) | vSAN (`vsanDatastore`) |
| NFS datastore under test | `nfs2` = `datastore-49` | `nfs2` = `datastore-38` |
| Enhanced Linked Mode | No (independent SSO domains) | |

Client: govmomi v0.52.0 (Go), calling `CnsRelocateVolume` on the source
vCenter's CNS endpoint with a `ServiceLocator` built via
`cns.GetServiceLocatorInstance` (instance UUID, `https://<target-vc>:443/sdk`,
SSO credentials, SSL thumbprint of the target vCenter).

Volumes under test: standalone CNS block volumes (First Class Disks), 2 GiB,
detached (not attached to any VM), created via `CnsCreateVolume` with
Kubernetes container-cluster metadata.

## Reproduction

### Case 1 — target datastore MoRef ID absent on source → InvalidArgument

Request (source vCenter CNS endpoint):

```
CnsRelocateVolume(
  CnsBlockVolumeRelocateSpec{
    volumeId:  2b682d2a-d59a-45a4-845a-6d8d404bd2a7,   # on source vSAN
    datastore: Datastore:datastore-38,                  # target VC's "nfs2"
    serviceLocator: { url: https://<target-vc>:443/sdk,
                      instanceUuid: <target instance uuid>,
                      credential: <SSO user/password>,
                      sslThumbprint: <target thumbprint> }
  })
```

Result, synchronous, in < 1 s:

```
ServerFaultCode: A specified parameter was not correct: VolumeRelocateSpec.datastore
```

Source `vsanvcmgmtd.log` shows the request is rejected in 4 ms, before the
relocate spec is even logged and before any privilege check or VSLM/vpxd
involvement:

```
2026-08-11T14:36:24.211Z info vsanvcmgmtd ... sub=AdapterServer opID=c734c5cc]
    Invoking 'relocate' on 'cns-volume-manager' session '...' active 1/1
2026-08-11T14:36:24.214Z info vsanvcmgmtd ... sub=AdapterServer opID=c734c5cc]
    Finished 'relocate' on 'cns-volume-manager' (4 ms): error
```

Nothing related to this operation appears in source `vpxd.log` or in any log
on the target vCenter. The source vCenter has no `datastore-38` in its
inventory (its own `nfs2` is `datastore-49`).

### Case 2 — same MoRef ID exists on source (different datastore) → succeeds, lands on target's datastore

The source vCenter has `datastore-21` = local datastore
`150-…-esx-install-datastore`. The target vCenter has `datastore-21` = a
**different** local datastore, `219-…-esx-install-datastore`.

Same call as above but with `datastore: Datastore:datastore-21`:

* Validation passes.
* Source `vsanvcmgmtd.log` (opID `c734c60f`):

```
sub=CnsCatalogSvc opID=c734c60f] The datastore datastore-21 is granted
    priviledge for VSPHERE.LOCAL\Administrator          <-- checked against the
                                                            SOURCE's datastore-21
sub=CnsVolMgr    opID=c734c60f] Relocate Volume with spec: (vim.cns.VolumeRelocateSpec) ...
sub=FcdService   opID=c734c60f] Relocate disk begin: volumeId=2b682d2a-...
    destination datastore=vim.Datastore:datastore-21
sub=Workflow     opId=c734c60f] Successfully relocate disk: id is 2b682d2a-...
```

* Operation completes in ~3 s. The FCD lands on the **target** vCenter's
  `datastore-21` (`219-…-esx-install-datastore`) — verified via the target's
  vslm catalog (`govc disk.ls`). The volume ID is preserved.

So the privilege check ran against the source's `datastore-21`
(`150-…-esx-install`) while the data was placed on the target's
`datastore-21` (`219-…-esx-install`) — two unrelated datastores that merely
share a numeric ID.

### Case 3 — control: IDs coincide and refer to analogous datastores

Both vCenters happen to have `datastore-19` = `vsanDatastore`. Cross-vCenter
relocation of detached CNS volumes source-vSAN → target-vSAN works reliably
(3–4 s for a 2 GiB volume; volume ID preserved; source CNS/vslm entries
removed; target picks the FCD up asynchronously — see Appendix C).

## Expected behavior

Per the API contract, `CnsBlockVolumeRelocateSpec.datastore` for a
cross-vCenter relocation identifies a datastore **on the target vCenter**
(the vCenter named by `serviceLocator`). Expected:

* The source vCenter must not resolve/authorize that MoRef against its own
  inventory.
* Datastore existence and privileges should be validated on the **target**
  vCenter via the ServiceLocator session.
* A wrong/nonexistent target datastore should produce a clear fault
  referencing the target vCenter.

## Actual behavior

* Source-side `vsanvcmgmtd` rejects any MoRef ID not present in the source
  inventory (`InvalidArgument`), making many valid cross-vCenter relocations
  impossible.
* When the ID happens to exist on the source, authorization is evaluated
  against the wrong (source) object; placement is evaluated on the target.
  Besides being incorrect, this is a potential authorization bypass surface:
  privileges on an unrelated source datastore gate access to a target
  datastore.

## Impact

We are building an automated migration of OpenShift/Kubernetes clusters
(including their CSI/CNS persistent volumes) from vSphere 8.x vCenters to
VCF 9 vCenters. Cross-vCenter `CnsRelocateVolume` (available since 8.0U3) is
the only API we found that preserves the CNS volume ID (and hence the
Kubernetes PV `volumeHandle`) across vCenters. This defect means:

* Direct relocation to the desired target datastore works only by MoRef-ID
  luck; in general customer environments it will fail.
* The only workaround we found is a two-hop pipeline through a datastore
  mounted by both vCenters (intra-VC relocate → staging datastore →
  cross-VC relocate), which requires shared storage most customers do not
  have across VCF boundaries, and doubles data movement.

## Questions for Broadcom

1. Is the source-side resolution of `VolumeRelocateSpec.datastore` a known
   defect? Is there a fix or planned fix (vCenter 8.0.x / 9.x)?
2. Is there a supported way to specify the target datastore for cross-vCenter
   `CnsRelocateVolume` that does not depend on the MoRef ID existing on the
   source (e.g., by datastore URL or by pre-registering the target datastore)?
3. Is cross-vCenter `CnsRelocateVolume` of detached CNS volumes between
   independent SSO domains (no Enhanced Linked Mode) a supported
   configuration? If yes, for which source/target version matrix?
4. Is the asynchronous target-side CNS registration (Appendix C) the intended
   behavior, and is there an API to trigger/await it (e.g.
   `CnsSyncDatastore`)?

---

## Appendix A — client call details

The client invokes, against the source vCenter:

* `CnsQueryVolume` (sanity check, volume healthy/detached)
* `CnsRelocateVolume` with a single `CnsBlockVolumeRelocateSpec`
  (fields: `volumeId`, `datastore` [target VC MoRef], optional `profile`,
  `serviceLocator`), then waits on the returned CNS task.

The ServiceLocator is constructed from a live session to the target vCenter:
instance UUID from `ServiceContent.About.InstanceUuid`, URL
`https://<hostname>:443/sdk`, `ServiceLocatorNamePassword` credential, and
the SHA-1 thumbprint of the target vCenter certificate.

## Appendix B — misleading `NotSupportedHost` on ServiceLocator DNS failure

Separate issue, same API. If the source vCenter (vpxd) cannot resolve the
hostname in `serviceLocator.url`, the client receives:

```
CnsFault: "VSLM task failed"
  └─ SystemError
       └─ vim.fault.NotSupportedHost
```

Source `vpxd.log` shows the real error:

```
Failed to resolve address; <target-vc-fqdn>:443 ... Host not found (authoritative)
Failed to create SOAP stub adapter
MoVStorageObjectMgr: Not able to establish remote connection
```

Adding a hosts entry on the source vCenter appliance fixed it and the same
relocation then succeeded in ~3 s. Requests:

* surface a connection/name-resolution fault instead of `NotSupportedHost`;
* document that `serviceLocator.url` must be resolvable/reachable from the
  source vCenter server itself, not merely from the API client.

Note: in one earlier attempt with the same DNS misconfiguration, the
operation performed a full data copy for ~2m38s and only then failed with
the same fault at switchover (rolled back cleanly). So the point of first
remote connection appears to vary with the data path chosen.

## Appendix C — asynchronous target-side CNS registration

After a successful cross-vCenter relocation:

* the FCD is immediately visible in the target's vslm catalog
  (`RetrieveVStorageObject` / `govc disk.ls`);
* `CnsQueryVolume` on the target returns no result for ~30–60+ s, until a
  periodic catalog sync runs. Target `vsanvcmgmtd.log`:

```
sub="CnsSync"] Process catalog change for FCD 2b682d2a-..., vclock = 2, change = objectModified
sub="Workflow"] Update volume 2b682d2a-...: added = true, ... isCnsVolume = true
```

(observed ~35 s after the relocate task completed). Initially the volume
reports `health: unknown`. If this is by design, please confirm the
recommended way for automation to await registration (poll `CnsQueryVolume`?
invoke `CnsSyncDatastore`?), and whether Kubernetes entity metadata carried
in the source CNS entry is preserved across the transfer.

## Appendix D — observed fault chain for Case 1 (client side)

```
error: invoking CnsRelocateVolume:
  ServerFaultCode: A specified parameter was not correct: VolumeRelocateSpec.datastore
```

(synchronous SOAP fault; no CNS task created; no target-side activity.)
