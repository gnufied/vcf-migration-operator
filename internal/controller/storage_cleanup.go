package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/vmware/govmomi/cns"
	cnstypes "github.com/vmware/govmomi/cns/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

const defaultRollbackRetention = 24 * time.Hour

func (r *VmwareCloudFoundationMigrationReconciler) ensureStorageSourceCleanup(ctx context.Context,
	migration *migrationv1alpha1.VmwareCloudFoundationMigration) (bool, ctrl.Result, error) {
	if !storageMigrationEnabled(migration) {
		return true, ctrl.Result{}, nil
	}
	copies := &migrationv1alpha1.VolumeCopyList{}
	if err := r.List(ctx, copies, client.MatchingLabels{storageMigrationUIDLabel: string(migration.UID)}); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("listing VolumeCopies for source cleanup: %w", err)
	}
	if len(copies.Items) == 0 {
		return true, ctrl.Result{}, nil
	}
	retention := migration.Spec.Storage.RollbackRetention.Duration
	if retention == 0 {
		retention = defaultRollbackRetention
	}
	for i := range copies.Items {
		copy := &copies.Items[i]
		if copy.Status.Phase != migrationv1alpha1.VolumeCopyPhaseSucceeded {
			return false, ctrl.Result{}, fmt.Errorf("VolumeCopy %s/%s is not successful", copy.Namespace, copy.Name)
		}
		if !migration.Spec.Storage.AcknowledgeSourceCleanup {
			if copy.Status.TransferCompletedAt == nil {
				return false, ctrl.Result{}, fmt.Errorf("VolumeCopy %s/%s has no authoritative completion time", copy.Namespace, copy.Name)
			}
			readyAt := copy.Status.TransferCompletedAt.Add(retention)
			if time.Now().Before(readyAt) {
				return false, ctrl.Result{RequeueAfter: minDuration(time.Until(readyAt), 30*time.Minute)}, nil
			}
		}
	}

	for i := range copies.Items {
		copy := &copies.Items[i]
		if copy.Status.SourcePV == nil {
			return false, ctrl.Result{}, fmt.Errorf("VolumeCopy %s/%s has no recorded source PV", copy.Namespace, copy.Name)
		}
		sourcePV := &corev1.PersistentVolume{}
		err := r.Get(ctx, client.ObjectKey{Name: copy.Status.SourcePV.Name}, sourcePV)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, ctrl.Result{}, fmt.Errorf("getting retained source PV %s: %w", copy.Status.SourcePV.Name, err)
		}
		if sourcePV.UID != copy.Status.SourcePV.UID || sourcePV.Spec.CSI == nil ||
			sourcePV.Spec.CSI.VolumeHandle != copy.Status.SourcePV.VolumeHandle {
			return false, ctrl.Result{}, fmt.Errorf("retained source PV %s identity changed", sourcePV.Name)
		}
		if copy.Spec.CutoverStrategy == migrationv1alpha1.VolumeCutoverStrategyPatchConsumer {
			transitioned, err := r.releasePatchedSourceClaim(ctx, copy, sourcePV)
			if err != nil {
				return false, ctrl.Result{}, err
			}
			if transitioned {
				return false, ctrl.Result{RequeueAfter: storageRequeueDelay}, nil
			}
		}
		if sourcePV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			return false, ctrl.Result{}, fmt.Errorf("retained source PV %s no longer has Retain policy", sourcePV.Name)
		}
		if sourcePV.Status.Phase == corev1.VolumeBound {
			return false, ctrl.Result{RequeueAfter: storageRequeueDelay}, nil
		}
		if err := r.deleteSourceCNSVolume(ctx, copy.Status.SourcePV.VolumeHandle); err != nil {
			return false, ctrl.Result{}, err
		}
		if err := r.Delete(ctx, sourcePV); err != nil && !apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, fmt.Errorf("deleting retained source PV %s: %w", sourcePV.Name, err)
		}
		r.Recorder.Eventf(migration, "Normal", "SourceVolumeDeleted",
			"Deleted retained source volume for VolumeCopy %s/%s after rollback retention", copy.Namespace, copy.Name)
		return false, ctrl.Result{RequeueAfter: storageRequeueDelay}, nil
	}
	return true, ctrl.Result{}, nil
}

func (r *VmwareCloudFoundationMigrationReconciler) releasePatchedSourceClaim(ctx context.Context,
	copy *migrationv1alpha1.VolumeCopy, sourcePV *corev1.PersistentVolume) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Spec.SourcePVC.Name}, pvc)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting source PVC %s/%s for cleanup: %w", copy.Namespace, copy.Spec.SourcePVC.Name, err)
	}
	if pvc.UID != copy.Spec.SourcePVC.UID || pvc.Spec.VolumeName != sourcePV.Name {
		return false, fmt.Errorf("source PVC %s/%s identity or binding changed", pvc.Namespace, pvc.Name)
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(copy.Namespace)); err != nil {
		return false, fmt.Errorf("listing source PVC consumers: %w", err)
	}
	for i := range pods.Items {
		for _, volume := range pods.Items[i].Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
				return false, fmt.Errorf("pod %s/%s still references source PVC %s", pods.Items[i].Namespace, pods.Items[i].Name, pvc.Name)
			}
		}
	}
	if sourcePV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		sourcePV.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		if err := r.Update(ctx, sourcePV); err != nil {
			return false, fmt.Errorf("retaining source PV %s: %w", sourcePV.Name, err)
		}
		return true, nil
	}
	confirmed := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: sourcePV.Name}, confirmed); err != nil {
		return false, fmt.Errorf("confirming source PV %s Retain policy: %w", sourcePV.Name, err)
	}
	if confirmed.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return false, fmt.Errorf("source PV %s Retain policy is not persisted", sourcePV.Name)
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting released source PVC %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}
	return true, nil
}

func (r *VmwareCloudFoundationMigrationReconciler) deleteSourceCNSVolume(ctx context.Context,
	volumeHandle string) error {
	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return fmt.Errorf("getting source vCenter for volume cleanup: %w", err)
	}
	if len(sourceVC.Datacenters) != 1 {
		return fmt.Errorf("source vCenter must have exactly one datacenter for volume cleanup")
	}
	sm := openshift.NewSecretManager(r.KubeClient)
	username, password, err := sm.GetCredentials(ctx, sourceVC.Server)
	if err != nil {
		return fmt.Errorf("getting source vCenter credentials for volume cleanup: %w", err)
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	session, err := getVSphereSession(cleanupCtx, sourceVC.Server, sourceVC.Datacenters[0], username, password)
	if err != nil {
		return fmt.Errorf("connecting to source vCenter for volume cleanup: %w", err)
	}
	cnsClient, err := cns.NewClient(cleanupCtx, session.Client.Client)
	if err != nil {
		return fmt.Errorf("creating source CNS client: %w", err)
	}
	id := cnstypes.CnsVolumeId{Id: volumeHandle}
	query, err := cnsClient.QueryVolume(cleanupCtx, &cnstypes.CnsQueryFilter{VolumeIds: []cnstypes.CnsVolumeId{id}})
	if err != nil {
		return fmt.Errorf("querying source CNS volume %s before deletion: %w", volumeHandle, err)
	}
	if len(query.Volumes) == 0 {
		return nil
	}
	task, err := cnsClient.DeleteVolume(cleanupCtx, []cnstypes.CnsVolumeId{id}, true)
	if err != nil {
		return fmt.Errorf("deleting source CNS volume %s: %w", volumeHandle, err)
	}
	taskInfo, err := cns.GetTaskInfo(cleanupCtx, task)
	if err != nil {
		return fmt.Errorf("waiting for source CNS volume %s deletion: %w", volumeHandle, err)
	}
	result, err := cns.GetTaskResult(cleanupCtx, taskInfo)
	if err != nil {
		return fmt.Errorf("reading source CNS volume %s deletion result: %w", volumeHandle, err)
	}
	if operationResult := result.GetCnsVolumeOperationResult(); operationResult.Fault != nil {
		return fmt.Errorf("deleting source CNS volume %s: %s", volumeHandle, operationResult.Fault.LocalizedMessage)
	}
	query, err = cnsClient.QueryVolume(cleanupCtx, &cnstypes.CnsQueryFilter{VolumeIds: []cnstypes.CnsVolumeId{id}})
	if err != nil {
		return fmt.Errorf("verifying source CNS volume %s deletion: %w", volumeHandle, err)
	}
	if len(query.Volumes) != 0 {
		return fmt.Errorf("source CNS volume %s still exists after deletion", volumeHandle)
	}
	return nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
