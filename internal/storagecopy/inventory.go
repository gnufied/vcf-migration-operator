package storagecopy

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

const VSphereCSIDriverName = "csi.vsphere.vmware.com"

// Backend is the selected storage migration implementation.
type Backend string

const (
	BackendNoMove   Backend = "NoMove"
	BackendFileCopy Backend = "FileCopy"
	BackendCNSRelay Backend = "CNSRelay"
	BackendManual   Backend = "Manual"
)

// ClassifyVolume selects a backend from the actual PV driver and volume mode.
func ClassifyVolume(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume,
	strategy migrationv1alpha1.StorageMigrationStrategy) (Backend, error) {
	if pvc == nil || pv == nil {
		return BackendManual, fmt.Errorf("PVC and PV must not be nil")
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName != pv.Name {
		return BackendManual, fmt.Errorf("PVC %s/%s is not bound to PV %s", pvc.Namespace, pvc.Name, pv.Name)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != VSphereCSIDriverName {
		return BackendNoMove, nil
	}

	mode := corev1.PersistentVolumeFilesystem
	if pvc.Spec.VolumeMode != nil {
		mode = *pvc.Spec.VolumeMode
	}
	if mode == corev1.PersistentVolumeBlock {
		if strategy == migrationv1alpha1.StorageMigrationStrategyCopy {
			return BackendManual, fmt.Errorf("raw block PVC %s/%s cannot use file copy", pvc.Namespace, pvc.Name)
		}
		return BackendCNSRelay, nil
	}
	if mode != corev1.PersistentVolumeFilesystem {
		return BackendManual, fmt.Errorf("PVC %s/%s has unsupported volumeMode %q", pvc.Namespace, pvc.Name, mode)
	}
	if strategy == migrationv1alpha1.StorageMigrationStrategyCNSRelay {
		return BackendCNSRelay, nil
	}
	return BackendFileCopy, nil
}

// MappingForStorageClass returns the unique mapping for a source StorageClass.
func MappingForStorageClass(mappings []migrationv1alpha1.StorageClassMapping, source string) (*migrationv1alpha1.StorageClassMapping, error) {
	var found *migrationv1alpha1.StorageClassMapping
	for i := range mappings {
		if mappings[i].Source != source {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple storageClassMappings select source %q", source)
		}
		found = &mappings[i]
	}
	if found == nil {
		return nil, fmt.Errorf("no storageClassMapping selects source %q", source)
	}
	return found, nil
}
