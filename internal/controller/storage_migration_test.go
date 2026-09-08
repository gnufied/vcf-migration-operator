package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestWorkloadGroupForPVC(t *testing.T) {
	controller := true
	owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app-123", UID: "controller-uid", Controller: &controller}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "one", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-one"}}},
			{Name: "two", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-two"}}},
		}},
	}}
	one := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-one", Namespace: "ns", UID: types.UID("one")}}
	two := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-two", Namespace: "ns", UID: types.UID("two")}}
	if got, want := workloadGroupForPVC(one, pods), workloadGroupForPVC(two, pods); got != want {
		t.Fatalf("PVCs consumed by one workload have different groups: %q and %q", got, want)
	}
	idle := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: "ns", UID: types.UID("idle")}}
	if got := workloadGroupForPVC(idle, pods); got != "pvc-idle" {
		t.Fatalf("idle PVC group = %q, want pvc-idle", got)
	}
}
