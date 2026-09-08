package storagecopy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestClassifyVolume(t *testing.T) {
	filesystem := corev1.PersistentVolumeFilesystem
	block := corev1.PersistentVolumeBlock
	tests := []struct {
		name     string
		driver   string
		mode     *corev1.PersistentVolumeMode
		strategy migrationv1alpha1.StorageMigrationStrategy
		want     Backend
		wantErr  bool
	}{
		{name: "external storage", driver: "cephfs.csi.ceph.com", mode: &filesystem, want: BackendNoMove},
		{name: "filesystem", driver: VSphereCSIDriverName, mode: &filesystem, want: BackendFileCopy},
		{name: "raw block", driver: VSphereCSIDriverName, mode: &block, want: BackendCNSRelay},
		{name: "raw block forced copy", driver: VSphereCSIDriverName, mode: &block, strategy: migrationv1alpha1.StorageMigrationStrategyCopy, want: BackendManual, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv", VolumeMode: tt.mode}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: tt.driver}}}}
			got, err := ClassifyVolume(pvc, pv, tt.strategy)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ClassifyVolume error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ClassifyVolume = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMappingForStorageClassRejectsAmbiguity(t *testing.T) {
	_, err := MappingForStorageClass([]migrationv1alpha1.StorageClassMapping{{Source: "source"}, {Source: "source"}}, "source")
	if err == nil {
		t.Fatal("MappingForStorageClass succeeded for duplicate mapping")
	}
}
