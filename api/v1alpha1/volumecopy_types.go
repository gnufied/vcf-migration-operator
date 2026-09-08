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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// VolumeCopyPhase is the persisted phase of a resumable filesystem copy.
type VolumeCopyPhase string

const (
	VolumeCopyPhasePlanned        VolumeCopyPhase = "Planned"
	VolumeCopyPhaseTargetPrepared VolumeCopyPhase = "TargetPrepared"
	VolumeCopyPhasePreCopying     VolumeCopyPhase = "PreCopying"
	VolumeCopyPhasePreCopied      VolumeCopyPhase = "PreCopied"
	VolumeCopyPhaseQuiescing      VolumeCopyPhase = "Quiescing"
	VolumeCopyPhaseFinalCopying   VolumeCopyPhase = "FinalCopying"
	VolumeCopyPhaseCopied         VolumeCopyPhase = "Copied"
	VolumeCopyPhaseCuttingOver    VolumeCopyPhase = "CuttingOver"
	VolumeCopyPhaseValidating     VolumeCopyPhase = "Validating"
	VolumeCopyPhaseSucceeded      VolumeCopyPhase = "Succeeded"
	VolumeCopyPhaseFailed         VolumeCopyPhase = "Failed"
)

// VolumeRebindPhase records each crash-recoverable original-name rebind step.
type VolumeRebindPhase string

const (
	VolumeRebindPhaseSourceBound       VolumeRebindPhase = "SourceBound"
	VolumeRebindPhasePVsRetained       VolumeRebindPhase = "PVsRetained"
	VolumeRebindPhaseTargetReserved    VolumeRebindPhase = "TargetReserved"
	VolumeRebindPhaseSourcePVCDeleted  VolumeRebindPhase = "SourcePVCDeleted"
	VolumeRebindPhaseOriginalRecreated VolumeRebindPhase = "OriginalRecreated"
	VolumeRebindPhaseRebound           VolumeRebindPhase = "Rebound"
)

// LocalObjectIdentity identifies an object by immutable UID as well as name.
type LocalObjectIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// VolumeObjectIdentity records a PV and its backing volume handle.
type VolumeObjectIdentity struct {
	Name         string    `json:"name"`
	UID          types.UID `json:"uid"`
	VolumeHandle string    `json:"volumeHandle"`
}

// VolumePlacement records a positively verified node and vCenter placement.
type VolumePlacement struct {
	Node    string    `json:"node"`
	NodeUID types.UID `json:"nodeUID"`
	Machine string    `json:"machine"`
	VCenter string    `json:"vCenter"`
	Region  string    `json:"region,omitempty"`
	Zone    string    `json:"zone,omitempty"`
}

// WorkloadState records a mutation needed to quiesce or constrain a consumer.
type WorkloadState struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Replicas   *int32 `json:"replicas,omitempty"`
	// InversePatch is applied only during a safe metadata-only rollback.
	// +optional
	InversePatch []byte `json:"inversePatch,omitempty"`
}

// VolumeCopySpec defines one namespaced, resumable PVC copy.
type VolumeCopySpec struct {
	MigrationUID        types.UID           `json:"migrationUID"`
	WorkloadGroup       string              `json:"workloadGroup"`
	SourcePVC           LocalObjectIdentity `json:"sourcePVC"`
	TargetStorageClass  string              `json:"targetStorageClass"`
	TargetFailureDomain string              `json:"targetFailureDomain"`
	PreCopy             bool                `json:"preCopy,omitempty"`
	// +kubebuilder:validation:Enum=Restricted;Exact
	Fidelity FileCopyFidelity `json:"fidelity"`
	// +kubebuilder:validation:Enum=Adapter;PatchConsumer;RebindOriginalName
	CutoverStrategy VolumeCutoverStrategy `json:"cutoverStrategy"`
	Transfer        TransferSpec          `json:"transfer"`
}

// VolumeCopyStatus defines observed copy and cutover state.
type VolumeCopyStatus struct {
	Phase                 VolumeCopyPhase       `json:"phase,omitempty"`
	RebindPhase           VolumeRebindPhase     `json:"rebindPhase,omitempty"`
	SourcePlacement       *VolumePlacement      `json:"sourcePlacement,omitempty"`
	TargetPlacement       *VolumePlacement      `json:"targetPlacement,omitempty"`
	SourcePV              *VolumeObjectIdentity `json:"sourcePV,omitempty"`
	TargetPVC             *LocalObjectIdentity  `json:"targetPVC,omitempty"`
	TargetPV              *VolumeObjectIdentity `json:"targetPV,omitempty"`
	Attempt               int32                 `json:"attempt,omitempty"`
	BytesCopied           int64                 `json:"bytesCopied,omitempty"`
	FilesCopied           int64                 `json:"filesCopied,omitempty"`
	TransferPod           string                `json:"transferPod,omitempty"`
	TransferStartedAt     *metav1.Time          `json:"transferStartedAt,omitempty"`
	TransferCompletedAt   *metav1.Time          `json:"transferCompletedAt,omitempty"`
	EffectiveRsyncOptions []string              `json:"effectiveRsyncOptions,omitempty"`
	OriginalWorkloadState []WorkloadState       `json:"originalWorkloadState,omitempty"`
	Message               string                `json:"message,omitempty"`
	Conditions            []metav1.Condition    `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=volumecopies,scope=Namespaced,shortName=vcopy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Source PVC",type=string,JSONPath=`.spec.sourcePVC.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VolumeCopy is the Schema for topology-aware filesystem volume copies.
type VolumeCopy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeCopySpec   `json:"spec"`
	Status VolumeCopyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeCopyList contains a list of VolumeCopy resources.
type VolumeCopyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeCopy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeCopy{}, &VolumeCopyList{})
}

// IsTerminal reports whether the copy requires no further forward transitions.
func (s *VolumeCopyStatus) IsTerminal() bool {
	return s.Phase == VolumeCopyPhaseSucceeded || s.Phase == VolumeCopyPhaseFailed
}

// HasAccessMode reports whether the claim requests the given access mode.
func HasAccessMode(modes []corev1.PersistentVolumeAccessMode, wanted corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range modes {
		if mode == wanted {
			return true
		}
	}
	return false
}
