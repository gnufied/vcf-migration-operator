package storagecopy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestBuildRsyncOptions(t *testing.T) {
	precopy, err := BuildRsyncOptions(migrationv1alpha1.FileCopyFidelityRestricted, false, "")
	if err != nil {
		t.Fatalf("BuildRsyncOptions precopy: %v", err)
	}
	precopyOptions, _ := precopy.Options()
	assertOption(t, precopyOptions, "--sparse", true)
	assertOption(t, precopyOptions, "--delete", false)

	final, err := BuildRsyncOptions(migrationv1alpha1.FileCopyFidelityExact, true, "1024")
	if err != nil {
		t.Fatalf("BuildRsyncOptions final: %v", err)
	}
	finalOptions, _ := final.Options()
	for _, option := range []string{"--sparse", "--delete", "--checksum", "--delete-delay", "--owner", "--group", "--acls", "--xattrs", "--bwlimit=1024"} {
		assertOption(t, finalOptions, option, true)
	}
}

func TestVerifyDestructiveSyncGuardRejectsSameHandle(t *testing.T) {
	pv := func() *corev1.PersistentVolume {
		return &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "same"}}}}
	}
	if err := VerifyDestructiveSyncGuard(t.Context(), nil, "app", pv(), pv(), "target"); err == nil {
		t.Fatal("same volume handle accepted")
	}
}

func assertOption(t *testing.T, options []string, wanted string, present bool) {
	t.Helper()
	found := false
	for _, option := range options {
		if option == wanted {
			found = true
			break
		}
	}
	if found != present {
		t.Fatalf("option %q present = %v, want %v in %#v", wanted, found, present, options)
	}
}
