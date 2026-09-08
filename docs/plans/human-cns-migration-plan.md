# Topology aware CNS volume migration between vCenters


## Common changes

1. Add a new state called planning to migration CR defined in @vmwarecloudFoundationMigration file. This where all migration starts:

```
  state: Planning                  # NEW enum value
  storage:
    mode: Automatic
    strategy: Copy
    storageClassMappings:
    - source: thin-csi
      target: thin-csi

      failureDomainMappings:
      - source: source-a
        target: target-a
      - source: source-b
        target: target-b
      - source: source-c
        target: target-c

```

2. Once migration is in planning stage, create a `StorageWorkloadMigration` object for each namespace. It could be created in VMO's namespace I think for now, but jury is out there if they should exist in namespace they target:

```
apiVersion: migration.openshift.io/v1alpha1
kind: StorageWorkloadMigration             # NEW resource
metadata:
  name: payments-database ("with namespace of project being the name here simply")
  namespace: <vmo-namespace>
spec:
  state: Planning
  dataCleanup: "none"
```

Should we record pvc and workload/statefulset/deployment name in `StorageWorkloadMigration` object?

3. Add a `migration.openshift.io/target-<migration-uid>` label to all nodes in the target vcenter and machineset etc. 


## Non-Zonal clusters

For non-zonal clusters once we are done with `Planning` phase and customer changes this to `Running` phase then following stuff happens in order:

1. The VMO goes through the usual steps and enters `TargetCapacityReady` (meaning vms on target vCenter are ready)
2. VMO next enters `StorageMigrated` state:

	```text
	InfrastructurePrepared
	DestinationInitialized
	MultiSiteConfigured
	TargetCapacityReady        # target worker MachineSets exist and nodes are Ready
	StorageMigrated
	  PlanVolumes
	  PrepareTargets
	  PreCopy                  # optional and non-authoritative
	  QuiesceAndFinalCopy      # per workload group
	  CutOverAndValidate
	WorkloadMigrated           # roll remaining compute; drain/scale source workers
	SourceCleaned
	Ready
	```
3. Once VMO enters and finishes `PreCopy` of all volumes in a particular namespace, it marks corresponding `StorageWorkloadMigration`'s `.spec.state`  as "PreCopyFinished". At this point an event is emitted that asks customer to annotate their namespace with `openshift.io/node-selector: "migration.openshift.io/target=<migration-uid>"` annotation and turn off all running workloads that use the PVCs in that namespace.
4. Once VMO detects that all PVC using workloads have been turned off, it changes `StorageWorkloadMigration`'s `.spec.state` to `ReadyForFinalCopy`.
5. At that point VMO copies all the data of all PVCs in that namespace and once that is finished in-addition to updating `VolumeCopy` objects it sets `StorageWorkloadMigration` `.spec.state` to `DataCopied`.

6. An event is emitted in that namespace that asks customers to resume their workload. 
6.1. Validate the moved data and the workload.
7. Workload resumes using above node selector and `StorageWorkloadMigration` marked (via `.spec.state`) as `DataMigrationComplete`.
8. Rinse and repeat for each namespace which has vSphere PVCs.
9. Once all `StorageWorkloadMigration` enter `DataMigrationComplete` state, vmo should resume rest of migration workflow including deletion of workers in older cluster.
10. VMO should move each `StorageWorkloadMigration` object into `DataMigrationCleanup` state at this point. 
11. Ask customer to set `StorageWorkloadMigration`'s `.spec.dataCleanup` to approved. This should remove the data on source vCenter and restore migration related annotations from workload namespaces. 


## For zonal clusters

For non-zonal clusters once we are done with `Planning` phase and customer changes this to `Running` phase then following stuff happens in order:

1. Validate if `storageClassMapping` sufficiently covers all target failure domains. Migration should not proceed if mapping are invalid.
2. Ensure that `(openshift-region, openshift-zone)` tuple is unique between vCenters. 
3. Customers can additionally annotate a PVC with `migration.openshift.io/target-failure-domain: target-zone-b` to custom target a failure-domain.
4. Same as non-zonal steps, the VMO goes through the usual steps and enters `TargetCapacityReady` (meaning vms on target vCenter are ready)
5. VMO next enters `StorageMigrated` state.
6. As usual VMO creates `StorageWorkloadMigration` objects for each namespace that uses vSphere volumes.
7. When VMO enters `PreCopy` state, it decides the topology of the receiver pod by using the `storageClassMapping` and creates the rsync receiver pod with appropriate `nodeAffinity` so as it can receive data from source rsync. 
8. A new storageclass called `thin-csi-new` is used that spans all failure-domains of the cluster using tagged based policy.
9. Once `PreCopy` if finished and `StorageWorkloadMigration` enters `PrecopyFinished` state, At this point an event is emitted that asks customer to annotate their namespace with `openshift.io/node-selector: "migration.openshift.io/target=<migration-uid>"` annotation and turn off all running workloads that use the PVCs in that namespace.
4. Once VMO detects that all PVC using workloads have been turned off, it changes `StorageWorkloadMigration`'s `.spec.state` to `ReadyForFinalCopy`.
5. At that point VMO copies all the data of all PVCs in that namespace and once that is finished in-addition to updating `VolumeCopy` objects it sets `StorageWorkloadMigration` `.spec.state` to `DataCopied`.

6. An event is emitted in that namespace that asks customers to resume their workload. 
6.1. Validate the moved data and the workload.
7. Workload resumes using above node selector and `StorageWorkloadMigration` marked (via `.spec.state`) as `DataMigrationComplete`.
8. Rinse and repeat for each namespace which has vSphere PVCs.
9. Once all `StorageWorkloadMigration` enter `DataMigrationComplete` state, vmo should resume rest of migration workflow including deletion of workers in older cluster.
10. VMO should move each `StorageWorkloadMigration` object into `DataMigrationCleanup` state at this point. 
11. Ask customer to set `StorageWorkloadMigration`'s `.spec.dataCleanup` to approved. This should remove the data on source vCenter and restore migration related annotations from workload namespaces. 







