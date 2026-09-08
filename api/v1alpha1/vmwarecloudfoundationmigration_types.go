/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationState represents the overall state of the migration workflow.
type MigrationState string

const (
	// MigrationStatePending indicates the migration has not started.
	MigrationStatePending MigrationState = "Pending"
	// MigrationStateRunning indicates the migration is actively progressing.
	MigrationStateRunning MigrationState = "Running"
	// MigrationStatePaused indicates the migration is paused by the user.
	MigrationStatePaused MigrationState = "Paused"
)

// SingletonName is the only object name the operator will reconcile. Since a
// single OpenShift cluster can only ever have one active vCenter migration,
// this follows OpenShift's singleton config resource convention (e.g.
// infrastructures.config.openshift.io/cluster).
const SingletonName = "cluster"

// SecretReference references a secret by name and namespace.
type SecretReference struct {
	// Name is the secret name.
	Name string `json:"name"`

	// Namespace is the secret namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// StorageMigrationMode controls how filesystem volume migration is orchestrated.
type StorageMigrationMode string

const (
	// StorageMigrationModeAutomatic lets the operator quiesce supported workloads.
	StorageMigrationModeAutomatic StorageMigrationMode = "Automatic"
	// StorageMigrationModeManual requires users to quiesce workloads explicitly.
	StorageMigrationModeManual StorageMigrationMode = "Manual"
	// StorageMigrationModeSkip preserves the Phase 1 behavior for CSI volumes.
	StorageMigrationModeSkip StorageMigrationMode = "Skip"
)

// StorageMigrationStrategy selects the data mover used for a volume.
type StorageMigrationStrategy string

const (
	// StorageMigrationStrategyAuto selects a mover from the volume capabilities.
	StorageMigrationStrategyAuto StorageMigrationStrategy = "Auto"
	// StorageMigrationStrategyCopy copies filesystem data to a new target volume.
	StorageMigrationStrategyCopy StorageMigrationStrategy = "Copy"
	// StorageMigrationStrategyCNSRelay preserves a CNS volume identity.
	StorageMigrationStrategyCNSRelay StorageMigrationStrategy = "CNSRelay"
)

// FileCopyFidelity controls the metadata preserved by rsync.
type FileCopyFidelity string

const (
	// FileCopyFidelityRestricted preserves metadata supported by restricted SCC.
	FileCopyFidelityRestricted FileCopyFidelity = "Restricted"
	// FileCopyFidelityExact preserves owners, groups, ACLs and extended attributes.
	FileCopyFidelityExact FileCopyFidelity = "Exact"
)

// VolumeCutoverStrategy controls how consumers are switched to the target claim.
type VolumeCutoverStrategy string

const (
	// VolumeCutoverStrategyAdapter uses a component-specific cutover adapter.
	VolumeCutoverStrategyAdapter VolumeCutoverStrategy = "Adapter"
	// VolumeCutoverStrategyPatchConsumer patches supported pod templates.
	VolumeCutoverStrategyPatchConsumer VolumeCutoverStrategy = "PatchConsumer"
	// VolumeCutoverStrategyRebindOriginalName rebinds the original PVC name.
	VolumeCutoverStrategyRebindOriginalName VolumeCutoverStrategy = "RebindOriginalName"
)

// StorageClassMapping maps a source StorageClass to a target failure domain.
type StorageClassMapping struct {
	// Source is the source StorageClass name.
	Source string `json:"source"`
	// Target is the target StorageClass name.
	Target string `json:"target"`
	// TargetFailureDomain names an entry in spec.failureDomains.
	TargetFailureDomain string `json:"targetFailureDomain"`
	// CutoverStrategy controls how consumers are switched to the copied volume.
	// +kubebuilder:validation:Enum=Adapter;PatchConsumer;RebindOriginalName
	// +kubebuilder:default=RebindOriginalName
	CutoverStrategy VolumeCutoverStrategy `json:"cutoverStrategy,omitempty"`
}

// TransferSpec configures the encrypted rsync transfer pods.
type TransferSpec struct {
	// Image is the digest-pinned rsync-transfer image.
	Image string `json:"image"`
	// BandwidthLimit is an optional rsync bandwidth limit such as "100Mi".
	// +optional
	BandwidthLimit string `json:"bandwidthLimit,omitempty"`
	// TargetTolerations are applied to target receiver pods.
	// +optional
	TargetTolerations []corev1.Toleration `json:"targetTolerations,omitempty"`
	// SchedulerName optionally selects the scheduler used by transfer pods.
	// +optional
	SchedulerName string `json:"schedulerName,omitempty"`
}

// StorageMigrationSpec configures topology-aware volume migration.
type StorageMigrationSpec struct {
	// Mode controls workload quiescing and whether storage migration is enabled.
	// +kubebuilder:validation:Enum=Automatic;Manual;Skip
	// +kubebuilder:default=Automatic
	Mode StorageMigrationMode `json:"mode,omitempty"`
	// Strategy selects the preferred volume migration backend.
	// +kubebuilder:validation:Enum=Auto;Copy;CNSRelay
	// +kubebuilder:default=Auto
	Strategy StorageMigrationStrategy `json:"strategy,omitempty"`
	// MaxConcurrentCopies bounds active copy attempts.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MaxConcurrentCopies int32 `json:"maxConcurrentCopies,omitempty"`
	// PreCopy enables a non-authoritative online seed copy when safe.
	// +kubebuilder:default=true
	PreCopy *bool `json:"preCopy,omitempty"`
	// Fidelity controls filesystem metadata preservation.
	// +kubebuilder:validation:Enum=Restricted;Exact
	Fidelity FileCopyFidelity `json:"fidelity"`
	// RollbackRetention keeps the retained source volume after cutover.
	// +kubebuilder:default="24h"
	RollbackRetention metav1.Duration `json:"rollbackRetention,omitempty"`
	// AcknowledgeSourceCleanup permits source-volume deletion before the
	// rollback retention period expires.
	// +optional
	AcknowledgeSourceCleanup bool `json:"acknowledgeSourceCleanup,omitempty"`
	// StorageClassMappings select target provisioning topology and cutover.
	// +optional
	StorageClassMappings []StorageClassMapping `json:"storageClassMappings,omitempty"`
	// Transfer configures transfer pods and transport.
	Transfer TransferSpec `json:"transfer"`
}

// VolumeCopyReference identifies a namespaced VolumeCopy without duplicating its status.
type VolumeCopyReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// StorageMigrationStatus contains bounded aggregate file-copy progress.
type StorageMigrationStatus struct {
	Total     int32 `json:"total,omitempty"`
	Succeeded int32 `json:"succeeded,omitempty"`
	Failed    int32 `json:"failed,omitempty"`
	Active    int32 `json:"active,omitempty"`
	Pending   int32 `json:"pending,omitempty"`
	// VolumeCopies references the authoritative per-PVC resources.
	// +optional
	VolumeCopies []VolumeCopyReference `json:"volumeCopies,omitempty"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationSpec struct {
	// State controls the workflow: Pending, Running, Paused.
	// The reconciler only acts when State is Running.
	// +kubebuilder:validation:Enum=Pending;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state"`

	// TargetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// FailureDomains defines failure domains for the target vCenter.
	// Uses OpenShift's standard VSpherePlatformFailureDomainSpec which includes
	// Name, Region, Zone, Server, and Topology with all necessary fields.
	// +kubebuilder:validation:MinItems=1
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// Storage configures topology-aware CSI volume migration. When omitted, the
	// legacy Skip behavior is retained for backwards compatibility.
	// +optional
	Storage *StorageMigrationSpec `json:"storage,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// Conditions represent the current state of the migration.
	// Each condition corresponds to a stage of the migration workflow.
	// Standard Kubernetes conditions using metav1.Condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StartTime is when the migration started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Storage contains aggregate progress; VolumeCopy resources contain details.
	// +optional
	Storage *StorageMigrationStatus `json:"storage,omitempty"`
}

// Condition type constants for the migration workflow.
// The reconciler checks conditions in this order; if a condition is not True,
// it executes the work for that condition and returns with RequeueAfter.
const (
	// ConditionInfrastructurePrepared indicates preflight checks passed and the
	// migration path has been selected.
	ConditionInfrastructurePrepared = "InfrastructurePrepared"

	// ConditionDestinationInitialized indicates the target vCenter has all required assets
	// (VM folders, region/zone tags).
	ConditionDestinationInitialized = "DestinationInitialized"

	// ConditionMultiSiteConfigured indicates the cluster recognizes both vCenters
	// (secrets, Infrastructure CRD, cloud-provider-config updated, pods restarted).
	ConditionMultiSiteConfigured = "MultiSiteConfigured"

	// ConditionStorageMigrated indicates selected persistent volumes have been
	// copied and their consumers cut over to target-vCenter nodes.
	ConditionStorageMigrated = "StorageMigrated"

	// ConditionWorkloadMigrated indicates compute is running in the new location
	// (new workers created, control plane rolled out, old MachineSets scaled to 0).
	ConditionWorkloadMigrated = "WorkloadMigrated"

	// ConditionSourceCleaned indicates the old vCenter is fully detached
	// (removed from Infrastructure, config, and secrets; CVO is re-enabled when
	// required by the legacy path).
	ConditionSourceCleaned = "SourceCleaned"

	// ConditionReady indicates migration is 100% complete.
	// This is an aggregate condition: all operators healthy, only target vCenters in Infrastructure.
	ConditionReady = "Ready"

	// ConditionAccepted indicates whether this object is the singleton instance
	// (named SingletonName) that the operator will act on.
	ConditionAccepted = "Accepted"
)

// Condition reason constants.
const (
	ReasonProgressing = "Progressing"
	ReasonCompleted   = "Completed"
	ReasonFailed      = "Failed"
	ReasonPaused      = "Paused"
	ReasonPending     = "Pending"

	// ReasonUnsupportedName indicates the object's name is not SingletonName,
	// so the operator is ignoring it.
	ReasonUnsupportedName = "UnsupportedName"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vmwarecloudfoundationmigrations,scope=Namespaced,shortName=vcfm
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VmwareCloudFoundationMigration is the Schema for the vmwarecloudfoundationmigrations API.
// It orchestrates migration of an OpenShift cluster from one vCenter to another.
type VmwareCloudFoundationMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VmwareCloudFoundationMigrationSpec   `json:"spec,omitempty"`
	Status VmwareCloudFoundationMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VmwareCloudFoundationMigrationList contains a list of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VmwareCloudFoundationMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VmwareCloudFoundationMigration{}, &VmwareCloudFoundationMigrationList{})
}
