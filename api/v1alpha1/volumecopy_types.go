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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeCopySpec identifies the source PVC whose data is migrated.
type VolumeCopySpec struct {
	// SourcePVC identifies the source PVC in this namespace.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sourcePVC is immutable"
	SourcePVC MigrationObjectReference `json:"sourcePVC"`

	// TargetFailureDomain is the resolved target failure domain, naming an entry in the
	// VmwareCloudFoundationMigration's spec.failureDomains.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetFailureDomain is immutable"
	TargetFailureDomain string `json:"targetFailureDomain"`

	// DataMigrationStrategy is the strategy VMO resolved for this PVC during Planning.
	// It is never Auto, and it is immutable so an in-flight copy never switches mechanism.
	// +required
	// +kubebuilder:validation:XValidation:rule="self != 'Auto'",message="dataMigrationStrategy must be a resolved strategy, not Auto"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="dataMigrationStrategy is immutable"
	DataMigrationStrategy VolumeDataMigrationStrategy `json:"dataMigrationStrategy"`
}

// VolumeCopyPhase is the persisted phase of a resumable volume migration.
// +kubebuilder:validation:Enum=Planned;TargetPrepared;PreCopying;PreCopied;FinalCopying;Copied;CuttingOver;Succeeded;Failed
type VolumeCopyPhase string

const (
	// VolumeCopyPhasePlanned indicates the copy is planned but no target exists yet.
	VolumeCopyPhasePlanned VolumeCopyPhase = "Planned"

	// VolumeCopyPhaseTargetPrepared indicates the target volume exists and its ownership is verified.
	VolumeCopyPhaseTargetPrepared VolumeCopyPhase = "TargetPrepared"

	// VolumeCopyPhasePreCopying indicates a non-authoritative seed copy is in progress.
	VolumeCopyPhasePreCopying VolumeCopyPhase = "PreCopying"

	// VolumeCopyPhasePreCopied indicates a non-authoritative seed copy completed.
	VolumeCopyPhasePreCopied VolumeCopyPhase = "PreCopied"

	// VolumeCopyPhaseFinalCopying indicates the authoritative copy is in progress.
	VolumeCopyPhaseFinalCopying VolumeCopyPhase = "FinalCopying"

	// VolumeCopyPhaseCopied indicates the authoritative copy completed.
	VolumeCopyPhaseCopied VolumeCopyPhase = "Copied"

	// VolumeCopyPhaseCuttingOver indicates the source PVC name is being rebound to the target volume.
	VolumeCopyPhaseCuttingOver VolumeCopyPhase = "CuttingOver"

	// VolumeCopyPhaseSucceeded indicates the volume migration completed.
	VolumeCopyPhaseSucceeded VolumeCopyPhase = "Succeeded"

	// VolumeCopyPhaseFailed indicates an unrecoverable error requires explicit recovery.
	VolumeCopyPhaseFailed VolumeCopyPhase = "Failed"
)

// VolumeCopyStatus contains operator-owned progress for one volume.
type VolumeCopyStatus struct {
	// Phase identifies the current phase of the volume migration.
	// +optional
	Phase VolumeCopyPhase `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=volumecopies,scope=Namespaced,shortName=vcopy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Source PVC",type=string,JSONPath=`.spec.sourcePVC.name`
// +kubebuilder:printcolumn:name="Target FD",type=string,JSONPath=`.spec.targetFailureDomain`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.dataMigrationStrategy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VolumeCopy tracks the migration of one source PVC's data to a target-vCenter volume.
// It lives in the source PVC's namespace and is owned by that namespace's
// StorageWorkloadMigration.
type VolumeCopy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec VolumeCopySpec `json:"spec"`

	// +optional
	Status VolumeCopyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeCopyList contains a list of VolumeCopy resources.
type VolumeCopyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeCopy `json:"items"`
}
