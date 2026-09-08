package storagecopy

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var cnsVolumeInfoGVK = schema.GroupVersionKind{Group: "cns.vmware.com", Version: "v1alpha1", Kind: "CNSVolumeInfoList"}

// VerifyVolumeOwnership positively resolves a handle to exactly one expected vCenter.
func VerifyVolumeOwnership(ctx context.Context, c client.Client, namespace, handle, expectedVCenter string) error {
	owner, err := ResolveVolumeOwner(ctx, c, namespace, handle)
	if err != nil {
		return err
	}
	want := strings.ToLower(strings.TrimSuffix(expectedVCenter, "."))
	if owner != want {
		return fmt.Errorf("volume handle %q is owned by vCenter %q, not target vCenter %q", handle, owner, expectedVCenter)
	}
	return nil
}

// ResolveVolumeOwner returns the single vCenter positively recorded for a handle.
func ResolveVolumeOwner(ctx context.Context, c client.Client, namespace, handle string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(cnsVolumeInfoGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("listing CNSVolumeInfo resources in %s: %w", namespace, err)
	}
	owners := map[string]struct{}{}
	for i := range list.Items {
		volumeID, _, err := unstructured.NestedString(list.Items[i].Object, "spec", "volumeID")
		if err != nil || volumeID != handle {
			continue
		}
		server, _, err := unstructured.NestedString(list.Items[i].Object, "spec", "vCenterServer")
		if err != nil {
			return "", fmt.Errorf("reading CNSVolumeInfo %s/%s vCenterServer: %w", namespace, list.Items[i].GetName(), err)
		}
		if server != "" {
			owners[strings.ToLower(strings.TrimSuffix(server, "."))] = struct{}{}
		}
	}
	if len(owners) == 0 {
		return "", fmt.Errorf("volume handle %q has no CNSVolumeInfo ownership record", handle)
	}
	if len(owners) != 1 {
		return "", fmt.Errorf("volume handle %q has ambiguous CNSVolumeInfo ownership", handle)
	}
	for owner := range owners {
		return owner, nil
	}
	return "", fmt.Errorf("volume handle %q ownership could not be resolved", handle)
}

// VerifyDestructiveSyncGuard enforces the final --delete safety invariant.
func VerifyDestructiveSyncGuard(ctx context.Context, c client.Client, namespace string,
	sourcePV, targetPV *corev1.PersistentVolume, expectedVCenter string) error {
	if sourcePV == nil || sourcePV.Spec.CSI == nil || targetPV == nil || targetPV.Spec.CSI == nil {
		return fmt.Errorf("source and target PVs must both have CSI volume handles")
	}
	if sourcePV.Spec.CSI.VolumeHandle == targetPV.Spec.CSI.VolumeHandle {
		return fmt.Errorf("refusing destructive sync: source and target volume handles are identical")
	}
	return VerifyVolumeOwnership(ctx, c, namespace, targetPV.Spec.CSI.VolumeHandle, expectedVCenter)
}

// ConfirmRetainPolicy re-reads a PV and proves Retain persisted before PVC deletion.
func ConfirmRetainPolicy(ctx context.Context, c client.Client, name string) error {
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, pv); err != nil {
		return fmt.Errorf("re-reading PV %s: %w", name, err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return fmt.Errorf("PV %s reclaim policy is %s, not Retain", name, pv.Spec.PersistentVolumeReclaimPolicy)
	}
	return nil
}

// ReservePVForClaim reserves a retained target PV without making it Available.
func ReservePVForClaim(pv *corev1.PersistentVolume, namespace, name string) {
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: namespace, Name: name}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
}
