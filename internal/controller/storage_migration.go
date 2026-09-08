package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/storagecopy"
)

const (
	storageRequeueDelay      = 15 * time.Second
	storageMigrationUIDLabel = "migration.openshift.io/migration-uid"
)

// ensureStorageMigrated plans filesystem copies, ensures target capacity, and
// aggregates the authoritative namespaced VolumeCopy resources.
func (r *VmwareCloudFoundationMigrationReconciler) ensureStorageMigrated(ctx context.Context,
	migration *migrationv1alpha1.VmwareCloudFoundationMigration) (ctrl.Result, error) {
	condType := migrationv1alpha1.ConditionStorageMigrated
	if !storageMigrationEnabled(migration) {
		r.setCondition(migration, condType, metav1.ConditionTrue, "Skipped", "Storage migration is disabled")
		return ctrl.Result{}, nil
	}

	ready, result, err := r.ensureTargetCapacityReady(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "Waiting for target worker capacity")
		return result, nil
	}

	plans, err := r.planFileCopies(ctx, migration)
	if err != nil {
		return ctrl.Result{}, err
	}
	existing := &migrationv1alpha1.VolumeCopyList{}
	if err := r.List(ctx, existing, client.MatchingLabels{storageMigrationUIDLabel: string(migration.UID)}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing VolumeCopies: %w", err)
	}
	byKey := make(map[types.NamespacedName]*migrationv1alpha1.VolumeCopy, len(existing.Items))
	status := &migrationv1alpha1.StorageMigrationStatus{Total: int32(len(plans))}
	var copySlotsUsed int32
	for i := range existing.Items {
		copy := &existing.Items[i]
		byKey[types.NamespacedName{Namespace: copy.Namespace, Name: copy.Name}] = copy
		status.VolumeCopies = append(status.VolumeCopies, migrationv1alpha1.VolumeCopyReference{Namespace: copy.Namespace, Name: copy.Name})
		switch copy.Status.Phase {
		case migrationv1alpha1.VolumeCopyPhaseSucceeded:
			status.Succeeded++
		case migrationv1alpha1.VolumeCopyPhaseFailed:
			status.Failed++
		case migrationv1alpha1.VolumeCopyPhaseCopied,
			migrationv1alpha1.VolumeCopyPhaseCuttingOver,
			migrationv1alpha1.VolumeCopyPhaseValidating:
			status.Active++
		case "", migrationv1alpha1.VolumeCopyPhasePlanned:
			status.Pending++
			copySlotsUsed++
		default:
			status.Active++
			copySlotsUsed++
		}
	}
	sort.Slice(status.VolumeCopies, func(i, j int) bool {
		if status.VolumeCopies[i].Namespace != status.VolumeCopies[j].Namespace {
			return status.VolumeCopies[i].Namespace < status.VolumeCopies[j].Namespace
		}
		return status.VolumeCopies[i].Name < status.VolumeCopies[j].Name
	})
	migration.Status.Storage = status
	if status.Failed > 0 {
		return ctrl.Result{}, fmt.Errorf("%d VolumeCopy resource(s) failed; inspect their status before retrying", status.Failed)
	}
	if status.Succeeded == status.Total {
		r.setCondition(migration, condType, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, fmt.Sprintf("Migrated %d filesystem volume(s)", status.Total))
		r.Recorder.Event(migration, "Normal", "StorageMigrated", "Filesystem volumes migrated to target vCenter")
		return ctrl.Result{}, nil
	}

	limit := migration.Spec.Storage.MaxConcurrentCopies
	if limit <= 0 {
		limit = 2
	}
	if copySlotsUsed < limit {
		for _, plan := range plans {
			key := types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}
			if byKey[key] != nil {
				continue
			}
			if err := r.Create(ctx, plan); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("creating VolumeCopy %s: %w", key, err)
			}
			status.Pending++
			status.VolumeCopies = append(status.VolumeCopies, migrationv1alpha1.VolumeCopyReference{Namespace: plan.Namespace, Name: plan.Name})
			break
		}
	}
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing,
		fmt.Sprintf("Filesystem volume copies: %d/%d succeeded, %d active, %d pending", status.Succeeded, status.Total, status.Active, status.Pending))
	return ctrl.Result{RequeueAfter: storageRequeueDelay}, nil
}

func storageMigrationEnabled(migration *migrationv1alpha1.VmwareCloudFoundationMigration) bool {
	return migration.Spec.Storage != nil && migration.Spec.Storage.Mode != migrationv1alpha1.StorageMigrationModeSkip
}

func (r *VmwareCloudFoundationMigrationReconciler) planFileCopies(ctx context.Context,
	migration *migrationv1alpha1.VmwareCloudFoundationMigration) ([]*migrationv1alpha1.VolumeCopy, error) {
	pvList, err := r.KubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistent volumes: %w", err)
	}
	pvs := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	for i := range pvList.Items {
		pvs[pvList.Items[i].Name] = &pvList.Items[i]
	}
	pvcList, err := r.KubeClient.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistent volume claims: %w", err)
	}
	podList, err := r.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods for workload grouping: %w", err)
	}
	preCopy := true
	if migration.Spec.Storage.PreCopy != nil {
		preCopy = *migration.Spec.Storage.PreCopy
	}
	fidelity := migration.Spec.Storage.Fidelity
	if fidelity == "" {
		return nil, fmt.Errorf("spec.storage.fidelity must explicitly select Restricted or Exact")
	}
	strategy := migration.Spec.Storage.Strategy
	if strategy == "" {
		strategy = migrationv1alpha1.StorageMigrationStrategyAuto
	}
	plans := make([]*migrationv1alpha1.VolumeCopy, 0, len(pvcList.Items))
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		pv := pvs[pvc.Spec.VolumeName]
		if pv == nil {
			continue
		}
		backend, err := storagecopy.ClassifyVolume(pvc, pv, strategy)
		if err != nil {
			return nil, err
		}
		switch backend {
		case storagecopy.BackendNoMove:
			continue
		case storagecopy.BackendCNSRelay, storagecopy.BackendManual:
			return nil, fmt.Errorf("PVC %s/%s requires unsupported backend %s", pvc.Namespace, pvc.Name, backend)
		}
		sourceSC := ""
		if pvc.Spec.StorageClassName != nil {
			sourceSC = *pvc.Spec.StorageClassName
		}
		mapping, err := storagecopy.MappingForStorageClass(migration.Spec.Storage.StorageClassMappings, sourceSC)
		if err != nil {
			return nil, fmt.Errorf("planning PVC %s/%s: %w", pvc.Namespace, pvc.Name, err)
		}
		cutover := mapping.CutoverStrategy
		if cutover == "" {
			cutover = migrationv1alpha1.VolumeCutoverStrategyRebindOriginalName
		}
		name := volumeCopyName(pvc.Name, pvc.UID)
		plans = append(plans, &migrationv1alpha1.VolumeCopy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pvc.Namespace, Labels: map[string]string{storageMigrationUIDLabel: string(migration.UID)}}, Spec: migrationv1alpha1.VolumeCopySpec{
			MigrationUID: migration.UID, WorkloadGroup: workloadGroupForPVC(pvc, podList.Items), SourcePVC: migrationv1alpha1.LocalObjectIdentity{Name: pvc.Name, UID: pvc.UID}, TargetStorageClass: mapping.Target,
			TargetFailureDomain: mapping.TargetFailureDomain, PreCopy: preCopy, Fidelity: fidelity, CutoverStrategy: cutover, Transfer: migration.Spec.Storage.Transfer,
		}})
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].Namespace != plans[j].Namespace {
			return plans[i].Namespace < plans[j].Namespace
		}
		return plans[i].Name < plans[j].Name
	})
	return plans, nil
}

func workloadGroupForPVC(pvc *corev1.PersistentVolumeClaim, pods []corev1.Pod) string {
	controllers := map[string]struct{}{}
	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != pvc.Namespace {
			continue
		}
		usesClaim := false
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
				usesClaim = true
				break
			}
		}
		if !usesClaim {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			controllers["Pod/"+string(pod.UID)] = struct{}{}
			continue
		}
		controllers[owner.APIVersion+"/"+owner.Kind+"/"+string(owner.UID)] = struct{}{}
	}
	if len(controllers) == 0 {
		return "pvc-" + string(pvc.UID)
	}
	identities := make([]string, 0, len(controllers))
	for identity := range controllers {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	sum := sha256.Sum256([]byte(strings.Join(identities, "\n")))
	return fmt.Sprintf("workload-%x", sum[:8])
}

func volumeCopyName(pvcName string, uid types.UID) string {
	suffix := string(uid)
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	name := sanitizeRFC1123(pvcName + "-" + suffix)
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

func (r *VmwareCloudFoundationMigrationReconciler) ensureTargetCapacityReady(ctx context.Context,
	migration *migrationv1alpha1.VmwareCloudFoundationMigration) (bool, ctrl.Result, error) {
	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	infraID, err := infraMgr.GetInfrastructureID(ctx)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
	allExist := true
	for i := range migration.Spec.FailureDomains {
		if _, err := machineMgr.GetMachineSet(ctx, workerMachineSetName(infraID, migration.Spec.FailureDomains[i].Name)); err != nil {
			allExist = false
			break
		}
	}
	if !allExist {
		sourceSets, err := machineMgr.GetMachineSetsByVCenter(ctx, sourceVC.Server)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if len(sourceSets) == 0 {
			return false, ctrl.Result{}, fmt.Errorf("no source worker MachineSets found")
		}
		var total int32
		for _, ms := range sourceSets {
			if ms.Spec.Replicas != nil {
				total += *ms.Spec.Replicas
			}
		}
		perFD := total / int32(len(migration.Spec.FailureDomains))
		if perFD < 1 {
			perFD = 1
		}
		for i := range migration.Spec.FailureDomains {
			fd := &migration.Spec.FailureDomains[i]
			name := workerMachineSetName(infraID, fd.Name)
			if _, err := machineMgr.GetMachineSet(ctx, name); err == nil {
				continue
			}
			replicas := perFD
			if i == len(migration.Spec.FailureDomains)-1 {
				replicas = total - perFD*int32(i)
				if replicas < 1 {
					replicas = 1
				}
			}
			if _, err := machineMgr.CreateWorkerMachineSet(ctx, name, replicas, fd, sourceSets[0], infraID); err != nil {
				return false, ctrl.Result{}, err
			}
			return false, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}
	readiness, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, infraID)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	return readiness.Ready, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
