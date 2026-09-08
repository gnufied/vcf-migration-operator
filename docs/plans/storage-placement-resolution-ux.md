# Storage placement conflict resolution: API and user flow

Status: Proposed extension; not implemented by the current API/controller.
Date: 2026-09-04
Related design: [file-copy volume migration](file-copy-volume-migration.md).

## Decision

Make analysis, resolution, and execution separate operations. A customer can
record a scheduling change for cutover without applying it to the running
source workload. Migration starts only against a reviewed, executable plan.
Expected customer-action blockers are reported as conditions and structured
issues, not as terminal copy failures or log-only errors.

Use the existing migration CR for cluster-wide configuration and execution,
add a namespaced `WorkloadMigration` for each connected workload/PVC group,
and retain `VolumeCopy` for each individual data transfer. This is an API
proposal, not a description of fields already available.

The group resource is necessary for this proposal because quiesce, scheduling
changes, and resume must have one authoritative owner across several PVCs.
Independent approvals on VolumeCopies would allow inconsistent decisions.

## What the current checkout actually supports

| Existing API/behavior | Meaning today | Missing capability |
|---|---|---|
| `vcfm.spec.state: Pending/Running/Paused` | Only Running executes preflight; Pending and Paused return early | Analyze without automatically proceeding after preflight |
| `spec.failureDomains` | Defines target infrastructure topology | Does not translate application scheduling intent |
| `storage.storageClassMappings` | One source class maps to one target class/failure domain | Cannot override placement for individual PVCs sharing a class |
| `storage.mode: Manual` | Waits for source PVC annotation `migration.openshift.io/ready-to-migrate=true` and checks writers | Does not represent deferred scheduling edits or a customer resume handshake |
| `VolumeCopy.spec.workloadGroup` | Associates copies with a group identifier | No independently stored group decision or execution coordinator |
| `VolumeCopy.status.originalWorkloadState` | Holds replica counts for quiesce and resume | Not a customer-authored desired scheduling change |
| `status.storage` | Aggregate copy counts and references | No complete preflight issue report or approved plan identity |

Current file-copy preflight checks image, mappings and volume classification;
it does not implement the proposed full workload placement analysis. The
current mover also adds a single-hostname selector during resume. That must
be replaced by the destination-PV placement mechanism discussed in the main
design conversation before claiming template-preserving cutover.

With the unchanged API, the only general resolution loop is: fail preflight,
customer makes a live configuration/mapping correction, retry preflight.
There is no API contract for "apply this correction later while stopped".
Manual mode is not a bypass for that omission.

## Proposed API responsibilities

### Migration CR

Add `Planning` to `spec.state`. It performs inventory and analysis, writes
report/group resources, and does not create workers, change vCenter/CSI
configuration, provision volumes, launch copy probes, or mutate applications.
Pending remains inert. Planning is permitted before execution starts; use
Paused for an already executing migration, preserving active checkpoints.

Add `spec.storage.approvedPlanID`. Running authorizes execution only when it
matches the current Ready plan. Planning and this approval requirement apply
to the new storage workflow; preserve the existing non-storage workflow.

```yaml
# Partial migration object: credentials, failureDomains, transfer and fidelity
# remain configured using the existing fields.
spec:
  state: Planning                  # NEW enum value
  storage:
    mode: Automatic
    strategy: Copy
    approvedPlanID: ""             # NEW field
    storageClassMappings:
      - source: source-vsan
        target: target-vsan-a
        targetFailureDomain: target-a
        cutoverStrategy: RebindOriginalName
status:
  storage:
    plan:                          # NEW bounded summary
      id: sha256:example
      observedGeneration: 7
      workloadGroups: 8
      readyGroups: 7
      blockedGroups: 1
  conditions:
    - type: StoragePlanReady        # NEW condition
      status: "False"
      reason: WorkloadPlacementConflict
      message: "1 workload group needs a resolution; list WorkloadMigrations"
```

List WorkloadMigrations by migration UID for complete results; do not duplicate
an unbounded issue list in the singleton. Report all discoverable conflicts
in one analysis pass. If inventory is incomplete, report that fact and do not
publish a Ready plan. Watches and bounded polling refresh the report as
configuration changes, without requiring a retry annotation.

### WorkloadMigration: proposed namespaced group resource

The planner creates this before VolumeCopies, including when target selection
is unresolved. It discovers connected components of PVC consumers, including
controller templates for scaled-to-zero workloads. Shared PVCs merge groups;
StatefulSet ordinals may have different target domains within one group.

Operator-populated identity fields include migration UID, controller and
authoritative owner references with UIDs, and source PVC/PV identities.
Customers edit only the resolution fields. Admission/RBAC and reconciliation
must enforce those ownership boundaries. Reject identity replacement rather
than adopting another object with the same name.

Proposed resolution modes:

| Mode | Customer input | Execution responsibility |
|---|---|---|
| `Preserve` (default) | Optional per-PVC target overrides | VMO changes storage; workload scheduling configuration stays intact |
| `PatchAtCutover` | Exact scheduling patch on each supported controller | VMO applies the reviewed patch while the group is quiesced |
| `Adapter` | Registered adapter and typed desired configuration | Adapter changes its authoritative application API at the safe checkpoint |
| `CustomerManaged` | Desired post-cutover scheduling state and a concrete supported stop/resume procedure | Customer changes authoritative configuration at an explicit checkpoint; VMO observes and validates it |

Per-PVC target overrides contain source PVC name/UID, target StorageClass and
target failure domain. They take precedence over the source-class default.
The planner validates all volumes used by each Pod together. A storage-class
mapping must not force every ordinal of a multi-zone StatefulSet into one zone.

Example customer resolution on a planner-created group (partial object):

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: WorkloadMigration             # NEW resource
metadata:
  name: payments-database
  namespace: payments
spec:
  resolution:
    mode: PatchAtCutover
    patches:
      - targetRef:
          apiVersion: apps/v1
          kind: StatefulSet
          name: database
          uid: "observed-controller-uid"
        operations:
          - op: test
            path: /spec/template/spec/nodeSelector/topology.kubernetes.io~1zone
            value: source-a
          - op: replace
            path: /spec/template/spec/nodeSelector/topology.kubernetes.io~1zone
            value: target-a
```

For initial support, allow only test/add/remove/replace operations on
nodeSelector and nodeAffinity under supported Deployment/StatefulSet Pod
templates. Require expected old values or absence checks and reject overlapping
operations. This is not a general arbitrary-object patch facility. Other
constraints, such as inter-Pod affinity, topology spread, or application CR
placement fields, need an explicit adapter or customer-managed resolution.

Do not patch a generated controller while its owner or GitOps reconciler will
undo the change. The plan must identify a supported authoritative configuration
path or a verified reconciliation-suspension and configuration handoff procedure.
An unknown or unsuspendable owner remains a blocker. A selected mode alone
does not prove that procedure is executable.

Status records observed inputs, structured issues (code, object identity,
field path, current constraint, proposed target, available resolutions),
effective before/after changes, related VolumeCopies, group execution phase,
and conditions. Expected phases include Planning, Blocked, Ready, Preparing,
PreCopying, AwaitingQuiesce, Quiescing, FinalCopying, CuttingOver,
AwaitingPlacementUpdate, AwaitingResume, Validating, Succeeded, Failed.

## Customer journey

| Step / console action       | API interaction                                                                  | Observable result                                                                     |
|-----------------------------|----------------------------------------------------------------------------------|---------------------------------------------------------------------------------------|
| 1. Configure and Analyze    | Set migration state Planning                                                     | Application continues using source; complete inventory/report appears                 |
| 2. Open a blocking workload | GET its WorkloadMigration                                                        | PVCs, exact conflicting fields, desired target and authoritative owner shown          |
| 3. Save a resolution        | Edit mapping/target override or group resolution                                 | Preview updates; no application change is applied                                     |
| 4. Review                   | Read Ready group plans and migration plan ID                                     | Exact patches, source/target volumes, quiesce owner and expected downtime steps shown |
| 5. Start migration          | Set Running and approvedPlanID together                                          | Infrastructure preparation and target capacity creation begin                         |
| 6. Observe preparation      | Read group and VolumeCopy status                                                 | Target ownership/placement verified, optional precopy progresses                      |
| 7. Cut over                 | VMO executes automatic steps; customer acts only at a declared manual checkpoint | All group volumes and scheduling changes switch while writers remain stopped          |
| 8. Validate and finish      | Observe actual Pods/volume handles/readiness                                     | Group succeeds; source cleanup remains subject to all groups and retention gates      |

Proposed CLI equivalents (resource short name `wkm` is also new):

```sh
oc -n openshift-vcf-migration get vcfm cluster -o yaml
oc get wkm -A
oc -n payments get wkm payments-database -o yaml
oc -n payments edit wkm payments-database
```

After reviewing the newly Ready plan, the customer applies this partial change
to the migration, using the actual ID returned by status:

```yaml
spec:
  state: Running
  storage:
    approvedPlanID: sha256:reviewed-plan-id
```

The console and CLI use the same API. There is no separate console-only
"ignore failure" control, and customer tools never edit status to proceed.

## Execution and approval boundaries

The plan ID covers migration UID, storage/infrastructure configuration,
group membership and object UIDs, relevant source scheduling/configuration
fingerprints, per-PVC targets, effective resolutions, and adapter versions.
Exclude status churn, timestamps, Running/Paused state and the approval field
itself. Keep the approved snapshot separate from expected controller mutations
so VMO's own scale-down/rebind does not invalidate its approval.

Before execution, changed material inputs create a new plan ID and invalidate
the old approval. The user reviews the updated preview. Blocked plans cannot
be approved into executability. During execution, persist the approved group
snapshot and reject conflicting edits to it. Unexpected drift blocks new
disruptive actions and triggers reanalysis of the remaining work; do not replay
completed cutovers or destroy existing checkpoints to generate a fresh plan.

Planning checks the proposed target infrastructure configuration, not the
existence of target nodes that have not yet been created. Before any group is
quiesced, a second readiness gate verifies actual target Nodes via
Node -> Machine -> vCenter, target PVC ownership/affinity, and workload
compatibility. Where inputs are unsupported or cannot be established, remain
blocked. Resource availability can change: readiness is not a reservation or
a guarantee that subsequent application startup will succeed.

Use API dry-run for validation/admission of proposed edits. A dry-run request
is not persisted and therefore does not run scheduler placement; it cannot
prove schedulability. Evaluate placement separately, including every mounted
PV, selectors/affinity, taints, resources and supported inter-Pod constraints.
Unsupported scheduler semantics need a declared support limit, not an
optimistic Ready result. See [Kubernetes dry-run semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)
and [scheduling constraints](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/).

The group coordinator owns these ordered actions:

1. Revalidate approved inputs and readiness for every volume in the group.
2. Persist original workload/configuration state and establish the supported
   continuous writer freeze. Quiesce once for the whole group.
3. Wait for every required final copy to succeed while the freeze holds.
4. Switch all group PVC bindings/references and apply only the reviewed
   scheduling changes. Journal each action; Kubernetes does not provide a
   transaction across these objects. A restart resumes from that journal.
5. Verify all bindings and final scheduling configuration before releasing
   any consumer. Delete stale gated Pods. If a name-based admission freeze
   would also block the new same-name target claim, explicitly transition it
   under the still-quiesced controllers after bindings/configuration are
   verified; do not wait for target Pods that the guard itself cannot admit.
6. Resume once, then verify Pod placement, actual volume handles and workload
   readiness before marking the group and its VolumeCopies Succeeded.

VolumeCopies gain an immutable group reference and approved plan identity.
They perform per-volume movement/rebinding under group commands and never
independently scale or resume a shared workload. Creating all needed copy
records does not mean running them all at once; concurrency limits apply to
attempts, not discovery of group membership.

Destination PV affinity is the default placement enforcement. Where absent,
adding it must be validated for the supported OpenShift API before cutover.
Existing insufficient affinity needs an explicit supported alternative.
An unchanged workload must satisfy the target constraints; storage affinity
cannot override hardcoded source topology. Target vCenter labels used by PVs
must be maintained on replacement Nodes beyond the migration lifecycle.

## Customer-managed checkpoints

CustomerManaged is a planned procedure, not an acknowledgement that suppresses
checks. Quiesce mode and placement resolution are separate: a customer may
quiesce a workload whose scheduling configuration needs no changes.

1. At AwaitingQuiesce, the user sees the exact group, Pods/controllers and
   required suspension actions. They stop writers through the authoritative
   API. A proposed `spec.actions.quiescedFor` token identifies this group,
   approved plan and checkpoint; VMO also verifies observable quiescence.
2. VMO final-syncs and switches bindings while the workload stays stopped.
3. At AwaitingPlacementUpdate, VMO displays the previously reviewed target
   configuration and the authoritative object to edit. The customer applies
   it without enabling writers. VMO observes the expected configuration and
   target compatibility; a button or annotation alone cannot satisfy this gate.
4. At AwaitingResume, VMO publishes a fresh resume checkpoint only after every
   binding and placement check passes. The customer sets proposed
   `spec.actions.resumeRequestedFor` to that token. VMO transitions the freeze
   and reports ResumePermitted; the customer then resumes through the
   authoritative API. VMO validates the resulting workload before success.

Tokens prevent stale actions from releasing a later attempt. Explicitly
record that manual users can enable target writes once ResumePermitted is
published; from then on prohibit automatic metadata-only rollback unless a
stronger adapter independently proves no target writes occurred. The same
conservative boundary applies to automatic resume.

If the operator cannot model the customer's expected Pod configuration or
verify the stop/resume procedure, the group remains unsupported. For an
initial implementation it is acceptable to omit CustomerManaged and Adapter
execution and expose only Preserve and supported PatchAtCutover, provided
the other cases remain explicit preflight blockers.

## Failure and cancellation UX

Before starting: fix any group and analyze again; source workloads stay online.
Before quiesce: readiness failures wait/retry without stopping that workload.
After quiesce: failures preserve the freeze and show which action is needed;
do not silently resume against partly switched group storage.

Paused preserves checkpoints and quiescence; it does not mean rollback.
Deleting a plan or copy is not an abort operation. Finalizers retain recovery
records while bindings are incomplete or a guard still affects Pods.
A separate explicit recovery procedure must restore all source bindings and
source scheduling configuration before releasing the freeze on rollback.
After target write permission, recovery must account for target data changes.

One blocked group prevents the initial migration start. If new blockers appear
after execution began, completed groups remain on target and the remaining
pipeline waits; source cleanup never bypasses an outstanding source dependency.
There is no "skip and forget" resolution.

## Implementation and validation

1. Add Planning, plan identity/status, group CRD and report-only controller.
2. Implement full inventory/grouping, per-PVC overrides, placement issues and
   supported patch previews; leave source application objects untouched.
3. Enforce plan approval, drift detection, actual target readiness, and move
   quiesce/resume ownership out of VolumeCopy into the group coordinator.
4. Add automatic scheduling edits at cutover, group journaling, and actual
   workload validation. Add manual/adapter execution only with complete
   checkpoint and authoritative-owner contracts.
5. Expose the same reports/actions in the console.

Required tests include: analysis has no migration side effects; reports include
multiple blockers and scaled-to-zero consumers; same-class PVCs may select
different domains; approval is invalidated by relevant drift but not status
updates; unsupported or operator-reverted patches block; every group volume
finishes before resume; restart after each binding/patch action; same-name
guard release cannot deadlock; stale manual tokens cannot release a freeze;
failures do not mark a group Succeeded without live workload validation.
