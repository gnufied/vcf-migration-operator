package storagecopy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	machineclient "github.com/openshift/client-go/machine/clientset/versioned"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

const (
	volumeCopyFinalizer   = "migration.openshift.io/volume-copy-cleanup"
	manualReadyAnnotation = "migration.openshift.io/ready-to-migrate"
	workloadDeployment    = "Deployment"
	workloadStatefulSet   = "StatefulSet"
	requeueDelay          = 10 * time.Second
)

// +kubebuilder:rbac:groups=migration.openshift.io,resources=volumecopies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.openshift.io,resources=volumecopies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.openshift.io,resources=volumecopies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumes;persistentvolumeclaims;pods;services;secrets;configmaps;serviceaccounts;events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses;volumeattachments;csidrivers,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;replicasets;daemonsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=machine.openshift.io,resources=machines;machinesets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cns.vmware.com,resources=cnsvolumeinfoes,verbs=get;list;watch

// VolumeCopyReconciler drives one filesystem copy and cutover at a time.
type VolumeCopyReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	KubeClient    kubernetes.Interface
	MachineClient machineclient.Interface
	Recorder      record.EventRecorder
	Engine        *PVCTransferEngine
}

// Reconcile advances at most one persisted VolumeCopy transition.
func (r *VolumeCopyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	copy := &migrationv1alpha1.VolumeCopy{}
	if err := r.Get(ctx, req.NamespacedName, copy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !copy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(copy, volumeCopyFinalizer) {
			if err := r.cleanupTransferResources(ctx, copy); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(copy, volumeCopyFinalizer)
			return ctrl.Result{}, r.Update(ctx, copy)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(copy, volumeCopyFinalizer) {
		controllerutil.AddFinalizer(copy, volumeCopyFinalizer)
		return ctrl.Result{}, r.Update(ctx, copy)
	}

	migration, fd, err := r.resolveMigration(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if migration.Spec.State == migrationv1alpha1.MigrationStatePaused {
		return ctrl.Result{}, nil
	}
	if migration.Spec.State != migrationv1alpha1.MigrationStateRunning {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	var result ctrl.Result
	switch copy.Status.Phase {
	case "":
		err = r.initialize(ctx, copy, fd.Server, fd.Region, fd.Zone)
	case migrationv1alpha1.VolumeCopyPhasePlanned:
		result, err = r.prepareTarget(ctx, copy, fd.Server, fd.Region, fd.Zone)
	case migrationv1alpha1.VolumeCopyPhaseTargetPrepared:
		if copy.Spec.PreCopy && r.preCopyEligible(ctx, copy) {
			copy.Status.Attempt++
			copy.Status.Phase = migrationv1alpha1.VolumeCopyPhasePreCopying
			copy.Status.Message = "starting non-authoritative precopy"
		} else {
			copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseQuiescing
			copy.Status.Message = "precopy skipped; quiescing writers"
		}
	case migrationv1alpha1.VolumeCopyPhasePreCopying:
		result, err = r.reconcileTransfer(ctx, copy, false)
	case migrationv1alpha1.VolumeCopyPhasePreCopied:
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseQuiescing
		copy.Status.Message = "precopy completed; quiescing writers"
	case migrationv1alpha1.VolumeCopyPhaseQuiescing:
		result, err = r.quiesce(ctx, copy, migration.Spec.Storage.Mode)
	case migrationv1alpha1.VolumeCopyPhaseFinalCopying:
		result, err = r.reconcileTransfer(ctx, copy, true)
	case migrationv1alpha1.VolumeCopyPhaseCopied:
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseCuttingOver
		copy.Status.Message = "authoritative copy complete; cutting over"
	case migrationv1alpha1.VolumeCopyPhaseCuttingOver:
		result, err = r.cutOver(ctx, copy)
	case migrationv1alpha1.VolumeCopyPhaseValidating:
		result, err = r.validateAndResume(ctx, copy)
	case migrationv1alpha1.VolumeCopyPhaseSucceeded, migrationv1alpha1.VolumeCopyPhaseFailed:
		return ctrl.Result{}, nil
	default:
		err = fmt.Errorf("unknown VolumeCopy phase %q", copy.Status.Phase)
	}
	if err != nil {
		return result, err
	}
	if statusErr := r.Status().Update(ctx, copy); statusErr != nil {
		return ctrl.Result{}, fmt.Errorf("updating VolumeCopy status: %w", statusErr)
	}
	return result, nil
}

func (r *VolumeCopyReconciler) resolveMigration(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (*migrationv1alpha1.VmwareCloudFoundationMigration, *failureDomain, error) {
	list := &migrationv1alpha1.VmwareCloudFoundationMigrationList{}
	if err := r.List(ctx, list); err != nil {
		return nil, nil, fmt.Errorf("listing migrations: %w", err)
	}
	for i := range list.Items {
		migration := &list.Items[i]
		if migration.UID != copy.Spec.MigrationUID {
			continue
		}
		for j := range migration.Spec.FailureDomains {
			fd := &migration.Spec.FailureDomains[j]
			if fd.Name == copy.Spec.TargetFailureDomain {
				return migration, &failureDomain{Name: fd.Name, Server: fd.Server, Region: fd.Region, Zone: fd.Zone}, nil
			}
		}
		return nil, nil, fmt.Errorf("target failure domain %q not found", copy.Spec.TargetFailureDomain)
	}
	return nil, nil, fmt.Errorf("migration UID %q not found", copy.Spec.MigrationUID)
}

type failureDomain struct{ Name, Server, Region, Zone string }

func (r *VolumeCopyReconciler) initialize(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, targetServer, region, zone string) error {
	pvc, pv, err := r.sourceObjects(ctx, copy)
	if err != nil {
		return err
	}
	backend, err := ClassifyVolume(pvc, pv, migrationv1alpha1.StorageMigrationStrategyCopy)
	if err != nil || backend != BackendFileCopy {
		return fmt.Errorf("source volume is not eligible for file copy: %w", err)
	}
	owner, err := ResolveVolumeOwner(ctx, r.Client, copy.Namespace, pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return err
	}
	placements, err := r.placements(ctx)
	if err != nil {
		return err
	}
	source, err := selectSourcePlacement(ctx, r.Client, pvc, placements, owner)
	if err != nil {
		return err
	}
	target, err := SelectTargetNode(placements, targetServer, region, zone)
	if err != nil {
		return err
	}
	sc, err := r.KubeClient.StorageV1().StorageClasses().Get(ctx, copy.Spec.TargetStorageClass, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting target StorageClass %s: %w", copy.Spec.TargetStorageClass, err)
	}
	if err := ValidateTargetStorageClass(sc, region, zone); err != nil {
		return err
	}
	copy.Status.SourcePV = &migrationv1alpha1.VolumeObjectIdentity{Name: pv.Name, UID: pv.UID, VolumeHandle: pv.Spec.CSI.VolumeHandle}
	copy.Status.SourcePlacement = placementStatus(source)
	copy.Status.TargetPlacement = placementStatus(target)
	copy.Status.Phase = migrationv1alpha1.VolumeCopyPhasePlanned
	copy.Status.Message = "source ownership and source/target placement verified"
	return nil
}

func (r *VolumeCopyReconciler) prepareTarget(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, targetServer, region, zone string) (ctrl.Result, error) {
	sourcePVC, _, err := r.sourceObjects(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	name := targetPVCName(sourcePVC, copy.Spec.MigrationUID)
	target := &corev1.PersistentVolumeClaim{}
	err = r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: name}, target)
	if apierrors.IsNotFound(err) {
		target = buildTargetPVC(sourcePVC, name, copy.Spec.TargetStorageClass, copy)
		if err := r.Create(ctx, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating target PVC: %w", err)
		}
		copy.Status.TargetPVC = &migrationv1alpha1.LocalObjectIdentity{Name: target.Name, UID: target.UID}
		copy.Status.Message = "target PVC created"
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting target PVC: %w", err)
	}
	if copy.Status.TargetPVC != nil && copy.Status.TargetPVC.UID != "" && copy.Status.TargetPVC.UID != target.UID {
		return ctrl.Result{}, fmt.Errorf("target PVC %s/%s UID changed", target.Namespace, target.Name)
	}
	copy.Status.TargetPVC = &migrationv1alpha1.LocalObjectIdentity{Name: target.Name, UID: target.UID}
	if err := r.ensureBindingPod(ctx, copy, target); err != nil {
		return ctrl.Result{}, err
	}
	if target.Status.Phase != corev1.ClaimBound || target.Spec.VolumeName == "" {
		copy.Status.Message = "waiting for target PVC to bind"
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: target.Spec.VolumeName}, pv); err != nil {
		return ctrl.Result{}, err
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != VSphereCSIDriverName {
		return ctrl.Result{}, fmt.Errorf("target PV %s is not vSphere CSI", pv.Name)
	}
	if pv.Spec.CSI.VolumeHandle == copy.Status.SourcePV.VolumeHandle {
		return ctrl.Result{}, fmt.Errorf("target PV reuses source volume handle")
	}
	if err := VerifyVolumeOwnership(ctx, r.Client, copy.Namespace, pv.Spec.CSI.VolumeHandle, targetServer); err != nil {
		copy.Status.Message = "waiting for positive target CNS ownership verification"
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if copy.Status.TargetPlacement.Region != region || copy.Status.TargetPlacement.Zone != zone {
		return ctrl.Result{}, fmt.Errorf("recorded target topology changed")
	}
	copy.Status.TargetPV = &migrationv1alpha1.VolumeObjectIdentity{Name: pv.Name, UID: pv.UID, VolumeHandle: pv.Spec.CSI.VolumeHandle}
	bindingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: stableResourceName("vcopy-bind", copy.Name), Namespace: copy.Namespace}}
	if err := r.Delete(ctx, bindingPod); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("deleting target binding pod: %w", err)
	}
	copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseTargetPrepared
	copy.Status.Message = "target volume bound and target ownership verified"
	return ctrl.Result{}, nil
}

func (r *VolumeCopyReconciler) reconcileTransfer(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, final bool) (ctrl.Result, error) {
	if err := r.revalidatePlacements(ctx, copy); err != nil {
		return ctrl.Result{}, err
	}
	sourcePVC, sourcePV, err := r.sourceObjects(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	targetPVC, targetPV, err := r.targetObjects(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if final {
		if err := VerifyDestructiveSyncGuard(ctx, r.Client, copy.Namespace, sourcePV, targetPV, copy.Status.TargetPlacement.VCenter); err != nil {
			return ctrl.Result{}, err
		}
		writers, err := r.writerPods(ctx, copy.Namespace, sourcePVC.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(writers) != 0 {
			return ctrl.Result{}, fmt.Errorf("writers reappeared before final sync: %s", strings.Join(writers, ", "))
		}
	}
	ready, err := r.Engine.EnsureAttempt(ctx, copy, sourcePVC, targetPVC, copy.Status.SourcePlacement.Node, copy.Status.TargetPlacement.Node, final)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	status, err := r.Engine.Status(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	copy.Status.TransferPod = status.PodName
	copy.Status.TransferStartedAt = status.StartedAt
	if !status.Completed {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	copy.Status.TransferCompletedAt = status.FinishedAt
	if !status.Succeeded {
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseFailed
		copy.Status.Message = fmt.Sprintf("rsync attempt %d failed with exit code %d", copy.Status.Attempt, status.ExitCode)
		return ctrl.Result{}, nil
	}
	opts, _ := BuildRsyncOptions(copy.Spec.Fidelity, final, copy.Spec.Transfer.BandwidthLimit)
	copy.Status.EffectiveRsyncOptions, _ = opts.Options()
	if final {
		writers, err := r.writerPods(ctx, copy.Namespace, sourcePVC.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(writers) != 0 {
			return ctrl.Result{}, fmt.Errorf("writers appeared during final sync: %s", strings.Join(writers, ", "))
		}
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseCopied
		copy.Status.Message = "authoritative final sync completed"
	} else {
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhasePreCopied
		copy.Status.Message = "non-authoritative precopy completed"
	}
	return ctrl.Result{}, nil
}

func (r *VolumeCopyReconciler) revalidatePlacements(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) error {
	placements, err := r.placements(ctx)
	if err != nil {
		return err
	}
	sourceValid := false
	targetValid := false
	for i := range placements {
		placement := &placements[i]
		if copy.Status.SourcePlacement != nil && placement.NodeName == copy.Status.SourcePlacement.Node &&
			placement.NodeUID == string(copy.Status.SourcePlacement.NodeUID) {
			sourceValid = placement.Ready && placement.Machine == copy.Status.SourcePlacement.Machine &&
				placement.VCenter == copy.Status.SourcePlacement.VCenter && placement.Region == copy.Status.SourcePlacement.Region &&
				placement.Zone == copy.Status.SourcePlacement.Zone
		}
		if copy.Status.TargetPlacement != nil && placement.NodeName == copy.Status.TargetPlacement.Node &&
			placement.NodeUID == string(copy.Status.TargetPlacement.NodeUID) {
			targetValid = placement.Ready && placement.Schedulable && placement.Machine == copy.Status.TargetPlacement.Machine &&
				placement.VCenter == copy.Status.TargetPlacement.VCenter && placement.Region == copy.Status.TargetPlacement.Region &&
				placement.Zone == copy.Status.TargetPlacement.Zone
		}
	}
	if !sourceValid {
		return fmt.Errorf("recorded source node placement is no longer valid")
	}
	if !targetValid {
		return fmt.Errorf("recorded target node placement is no longer valid")
	}
	return nil
}

func (r *VolumeCopyReconciler) quiesce(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, mode migrationv1alpha1.StorageMigrationMode) (ctrl.Result, error) {
	pvc, _, err := r.sourceObjects(ctx, copy)
	if err != nil {
		return ctrl.Result{}, err
	}
	writers, err := r.writerPods(ctx, copy.Namespace, pvc.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mode == migrationv1alpha1.StorageMigrationModeManual {
		if pvc.Annotations[manualReadyAnnotation] != "true" {
			copy.Status.Message = "waiting for manual ready annotation"
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if len(writers) != 0 {
			return ctrl.Result{}, fmt.Errorf("manual quiesce asserted but writer pods remain: %s", strings.Join(writers, ", "))
		}
	} else if len(copy.Status.OriginalWorkloadState) == 0 && strings.HasPrefix(copy.Spec.WorkloadGroup, "workload-") {
		states, err := r.discoverWorkloadState(ctx, copy.Namespace, pvc.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(states) == 0 {
			states, err = r.workloadStateFromGroup(ctx, copy)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		if len(states) == 0 {
			copy.Status.Message = "waiting to recover workload-group state before quiesce"
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		copy.Status.OriginalWorkloadState = states
		copy.Status.Message = "recorded original workload state before quiesce"
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	} else if len(writers) != 0 {
		changed, err := r.scaleRecordedWorkloads(ctx, copy, 0)
		if err != nil {
			return ctrl.Result{}, err
		}
		copy.Status.Message = "waiting for writer pods to terminate"
		if changed || len(writers) != 0 {
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
	}
	copy.Status.Attempt++
	copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseFinalCopying
	copy.Status.Message = "writers quiesced; starting authoritative final sync"
	return ctrl.Result{}, nil
}

func (r *VolumeCopyReconciler) workloadStateFromGroup(ctx context.Context,
	copy *migrationv1alpha1.VolumeCopy) ([]migrationv1alpha1.WorkloadState, error) {
	siblings := &migrationv1alpha1.VolumeCopyList{}
	if err := r.List(ctx, siblings, client.InNamespace(copy.Namespace), client.MatchingLabels{labelMigrationUID: string(copy.Spec.MigrationUID)}); err != nil {
		return nil, fmt.Errorf("listing workload-group VolumeCopies: %w", err)
	}
	for i := range siblings.Items {
		sibling := &siblings.Items[i]
		if sibling.Spec.WorkloadGroup == copy.Spec.WorkloadGroup && len(sibling.Status.OriginalWorkloadState) != 0 {
			return append([]migrationv1alpha1.WorkloadState(nil), sibling.Status.OriginalWorkloadState...), nil
		}
	}
	return nil, nil
}

func (r *VolumeCopyReconciler) cutOver(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (ctrl.Result, error) {
	switch copy.Spec.CutoverStrategy {
	case migrationv1alpha1.VolumeCutoverStrategyRebindOriginalName:
		return r.rebindOriginalName(ctx, copy)
	case migrationv1alpha1.VolumeCutoverStrategyPatchConsumer:
		return r.patchConsumers(ctx, copy)
	default:
		return ctrl.Result{}, fmt.Errorf("cutover strategy %q requires an adapter that is not configured", copy.Spec.CutoverStrategy)
	}
}

func (r *VolumeCopyReconciler) validateAndResume(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (ctrl.Result, error) {
	siblings := &migrationv1alpha1.VolumeCopyList{}
	if err := r.List(ctx, siblings, client.InNamespace(copy.Namespace), client.MatchingLabels{labelMigrationUID: string(copy.Spec.MigrationUID)}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing workload-group VolumeCopies: %w", err)
	}
	for i := range siblings.Items {
		sibling := &siblings.Items[i]
		if sibling.Spec.WorkloadGroup != copy.Spec.WorkloadGroup || sibling.UID == copy.UID {
			continue
		}
		if sibling.Status.Phase == migrationv1alpha1.VolumeCopyPhaseFailed {
			return ctrl.Result{}, fmt.Errorf("workload-group VolumeCopy %s/%s failed", sibling.Namespace, sibling.Name)
		}
		if sibling.Status.Phase != migrationv1alpha1.VolumeCopyPhaseValidating &&
			sibling.Status.Phase != migrationv1alpha1.VolumeCopyPhaseSucceeded {
			copy.Status.Message = "waiting for every workload-group volume to complete cutover"
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
	}
	changed, err := r.scaleRecordedWorkloads(ctx, copy, -1)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if err := r.cleanupTransferResources(ctx, copy); err != nil {
		return ctrl.Result{}, err
	}
	copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseSucceeded
	copy.Status.Message = "target volume cut over and workload resumed"
	r.Recorder.Event(copy, "Normal", "VolumeCopySucceeded", copy.Status.Message)
	return ctrl.Result{}, nil
}

func (r *VolumeCopyReconciler) sourceObjects(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Spec.SourcePVC.Name}, pvc); err != nil {
		return nil, nil, err
	}
	if pvc.UID != copy.Spec.SourcePVC.UID {
		return nil, nil, fmt.Errorf("source PVC UID changed")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return nil, nil, err
	}
	if copy.Status.SourcePV != nil && copy.Status.SourcePV.UID != "" && pv.UID != copy.Status.SourcePV.UID {
		return nil, nil, fmt.Errorf("source PV UID changed")
	}
	return pvc, pv, nil
}

func (r *VolumeCopyReconciler) targetObjects(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	if copy.Status.TargetPVC == nil || copy.Status.TargetPV == nil {
		return nil, nil, fmt.Errorf("target identities are not recorded")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Status.TargetPVC.Name}, pvc); err != nil {
		return nil, nil, err
	}
	if pvc.UID != copy.Status.TargetPVC.UID {
		return nil, nil, fmt.Errorf("target PVC UID changed")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: copy.Status.TargetPV.Name}, pv); err != nil {
		return nil, nil, err
	}
	if pv.UID != copy.Status.TargetPV.UID {
		return nil, nil, fmt.Errorf("target PV UID changed")
	}
	return pvc, pv, nil
}

func (r *VolumeCopyReconciler) placements(ctx context.Context) ([]NodePlacement, error) {
	nodes, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	machines, err := r.MachineClient.MachineV1beta1().Machines("openshift-machine-api").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return ResolveNodePlacements(nodes.Items, machines.Items)
}

func selectSourcePlacement(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, placements []NodePlacement, owner string) (*NodePlacement, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if podUsesPVC(&pods.Items[i], pvc.Name) && pods.Items[i].Spec.NodeName != "" {
			for j := range placements {
				if placements[j].NodeName == pods.Items[i].Spec.NodeName && strings.EqualFold(placements[j].VCenter, owner) {
					return &placements[j], nil
				}
			}
		}
	}
	for i := range placements {
		if placements[i].Ready && placements[i].Schedulable && strings.EqualFold(placements[i].VCenter, owner) {
			return &placements[i], nil
		}
	}
	return nil, fmt.Errorf("no Ready source node maps to vCenter %q", owner)
}

func placementStatus(p *NodePlacement) *migrationv1alpha1.VolumePlacement {
	return &migrationv1alpha1.VolumePlacement{Node: p.NodeName, NodeUID: typesUID(p.NodeUID), Machine: p.Machine, VCenter: p.VCenter, Region: p.Region, Zone: p.Zone}
}

func typesUID(value string) types.UID { return types.UID(value) }

func targetPVCName(source *corev1.PersistentVolumeClaim, uid types.UID) string {
	sum := sha256.Sum256([]byte(string(uid) + "/" + string(source.UID)))
	base := strings.TrimSuffix(source.Name, "-")
	if len(base) > 42 {
		base = base[:42]
	}
	return fmt.Sprintf("%s-vcf-target-%x", base, sum[:4])
}

func buildTargetPVC(source *corev1.PersistentVolumeClaim, name, storageClass string, copy *migrationv1alpha1.VolumeCopy) *corev1.PersistentVolumeClaim {
	requests := source.Spec.Resources.Requests.DeepCopy()
	if capacity, ok := source.Status.Capacity[corev1.ResourceStorage]; ok {
		if requested, exists := requests[corev1.ResourceStorage]; !exists || capacity.Cmp(requested) > 0 {
			requests[corev1.ResourceStorage] = capacity.DeepCopy()
		}
	}
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: source.Namespace,
		Labels: transferLabels(copy, "target-pvc"), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(copy, migrationv1alpha1.GroupVersion.WithKind("VolumeCopy"))}},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: append([]corev1.PersistentVolumeAccessMode(nil), source.Spec.AccessModes...),
			VolumeMode: source.Spec.VolumeMode, StorageClassName: &storageClass, Resources: corev1.VolumeResourceRequirements{Requests: requests}}}
}

func (r *VolumeCopyReconciler) ensureBindingPod(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, pvc *corev1.PersistentVolumeClaim) error {
	name := stableResourceName("vcopy-bind", copy.Name)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: copy.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pod, func() error {
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(copy, migrationv1alpha1.GroupVersion.WithKind("VolumeCopy"))}
		if pod.CreationTimestamp.IsZero() {
			pod.Spec = corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, SchedulerName: copy.Spec.Transfer.SchedulerName,
				Tolerations: copy.Spec.Transfer.TargetTolerations,
				Affinity:    &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{copy.Status.TargetPlacement.Node}}}}}}}},
				Containers:  []corev1.Container{{Name: "bind", Image: copy.Spec.Transfer.Image, Command: []string{"/bin/bash", "-c", "trap : TERM INT; sleep infinity & wait"}, VolumeMounts: []corev1.VolumeMount{{Name: "target", MountPath: "/mnt/target"}}}},
				Volumes:     []corev1.Volume{{Name: "target", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}}}
		}
		return nil
	})
	return err
}

func (r *VolumeCopyReconciler) preCopyEligible(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) bool {
	pvc, _, err := r.sourceObjects(ctx, copy)
	if err != nil {
		return false
	}
	if migrationv1alpha1.HasAccessMode(pvc.Spec.AccessModes, corev1.ReadWriteOncePod) {
		return false
	}
	return true
}

func (r *VolumeCopyReconciler) writerPods(ctx context.Context, namespace, pvc string) ([]string, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var writers []string
	for i := range pods.Items {
		if !podUsesPVC(&pods.Items[i], pvc) || podTerminated(&pods.Items[i]) {
			continue
		}
		if podMountsPVCWritable(&pods.Items[i], pvc) {
			writers = append(writers, pods.Items[i].Namespace+"/"+pods.Items[i].Name)
		}
	}
	sort.Strings(writers)
	return writers, nil
}

func podUsesPVC(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}
func podTerminated(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}
func podMountsPVCWritable(pod *corev1.Pod, claim string) bool {
	volumeNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			if !v.PersistentVolumeClaim.ReadOnly {
				volumeNames[v.Name] = true
			}
		}
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if volumeNames[m.Name] && !m.ReadOnly {
				return true
			}
		}
	}
	return false
}

func (r *VolumeCopyReconciler) discoverWorkloadState(ctx context.Context, namespace, pvc string) ([]migrationv1alpha1.WorkloadState, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	states := map[string]migrationv1alpha1.WorkloadState{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podUsesPVC(pod, pvc) || podTerminated(pod) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil, fmt.Errorf("pod %s/%s using PVC %s has no scalable controller", pod.Namespace, pod.Name, pvc)
		}
		switch owner.Kind {
		case workloadStatefulSet:
			sts := &appsv1.StatefulSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, sts); err != nil {
				return nil, err
			}
			states[workloadStatefulSet+"/"+pod.Namespace+"/"+sts.Name] = migrationv1alpha1.WorkloadState{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: workloadStatefulSet, Namespace: pod.Namespace, Name: sts.Name, Replicas: sts.Spec.Replicas}
		case "ReplicaSet":
			rs := &appsv1.ReplicaSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, rs); err != nil {
				return nil, err
			}
			rsOwner := metav1.GetControllerOf(rs)
			if rsOwner == nil || rsOwner.Kind != workloadDeployment {
				return nil, fmt.Errorf("ReplicaSet %s/%s is not owned by a Deployment", rs.Namespace, rs.Name)
			}
			deployment := &appsv1.Deployment{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: rsOwner.Name}, deployment); err != nil {
				return nil, err
			}
			states[workloadDeployment+"/"+pod.Namespace+"/"+deployment.Name] = migrationv1alpha1.WorkloadState{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: workloadDeployment, Namespace: pod.Namespace, Name: deployment.Name, Replicas: deployment.Spec.Replicas}
		default:
			return nil, fmt.Errorf("pod %s/%s uses unsupported automatic quiesce controller %s", pod.Namespace, pod.Name, owner.Kind)
		}
	}
	result := make([]migrationv1alpha1.WorkloadState, 0, len(states))
	for _, state := range states {
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Namespace != result[j].Namespace {
			return result[i].Namespace < result[j].Namespace
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (r *VolumeCopyReconciler) scaleRecordedWorkloads(ctx context.Context, copy *migrationv1alpha1.VolumeCopy, replicas int32) (bool, error) {
	for i := range copy.Status.OriginalWorkloadState {
		state := &copy.Status.OriginalWorkloadState[i]
		desired := replicas
		if replicas < 0 {
			if state.Replicas == nil {
				continue
			}
			desired = *state.Replicas
		}
		switch state.Kind {
		case workloadDeployment:
			obj := &appsv1.Deployment{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: state.Namespace, Name: state.Name}, obj); err != nil {
				return false, err
			}
			placementChanged := false
			if replicas < 0 && obj.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] != copy.Status.TargetPlacement.Node {
				constrainToTarget(&obj.Spec.Template.Spec, copy.Status.TargetPlacement.Node)
				placementChanged = true
			}
			if !placementChanged && obj.Spec.Replicas != nil && *obj.Spec.Replicas == desired {
				continue
			}
			obj.Spec.Replicas = &desired
			if err := r.Update(ctx, obj); err != nil {
				return false, err
			}
			return true, nil
		case workloadStatefulSet:
			obj := &appsv1.StatefulSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: state.Namespace, Name: state.Name}, obj); err != nil {
				return false, err
			}
			placementChanged := false
			if replicas < 0 && obj.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] != copy.Status.TargetPlacement.Node {
				constrainToTarget(&obj.Spec.Template.Spec, copy.Status.TargetPlacement.Node)
				placementChanged = true
			}
			if !placementChanged && obj.Spec.Replicas != nil && *obj.Spec.Replicas == desired {
				continue
			}
			obj.Spec.Replicas = &desired
			if err := r.Update(ctx, obj); err != nil {
				return false, err
			}
			return true, nil
		default:
			return false, fmt.Errorf("unsupported recorded workload kind %q", state.Kind)
		}
	}
	return false, nil
}

//nolint:gocyclo // Explicit persisted states keep crash recovery auditable.
func (r *VolumeCopyReconciler) rebindOriginalName(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (ctrl.Result, error) {
	sourcePV := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: copy.Status.SourcePV.Name}, sourcePV); err != nil {
		return ctrl.Result{}, err
	}
	targetPV := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: copy.Status.TargetPV.Name}, targetPV); err != nil {
		return ctrl.Result{}, err
	}
	targetPVC := &corev1.PersistentVolumeClaim{}
	targetPVCErr := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Status.TargetPVC.Name}, targetPVC)
	if targetPVCErr != nil && !apierrors.IsNotFound(targetPVCErr) {
		return ctrl.Result{}, targetPVCErr
	}
	switch copy.Status.RebindPhase {
	case "":
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhaseSourceBound
		return ctrl.Result{}, nil
	case migrationv1alpha1.VolumeRebindPhaseSourceBound:
		if err := r.cleanupTransferResources(ctx, copy); err != nil {
			return ctrl.Result{}, err
		}
		attached, err := r.hasVolumeAttachment(ctx, targetPV.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		if attached {
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if sourcePV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			sourcePV.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			if err := r.Update(ctx, sourcePV); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if targetPV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
			targetPV.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			if err := r.Update(ctx, targetPV); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if err := ConfirmRetainPolicy(ctx, r.Client, sourcePV.Name); err != nil {
			return ctrl.Result{}, err
		}
		if err := ConfirmRetainPolicy(ctx, r.Client, targetPV.Name); err != nil {
			return ctrl.Result{}, err
		}
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhasePVsRetained
	case migrationv1alpha1.VolumeRebindPhasePVsRetained:
		if targetPVCErr == nil {
			if err := r.Delete(ctx, targetPVC); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		ReservePVForClaim(targetPV, copy.Namespace, copy.Spec.SourcePVC.Name)
		if err := r.Update(ctx, targetPV); err != nil {
			return ctrl.Result{}, err
		}
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhaseTargetReserved
	case migrationv1alpha1.VolumeRebindPhaseTargetReserved:
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Spec.SourcePVC.Name}, pvc)
		if err == nil {
			if pvc.UID != copy.Spec.SourcePVC.UID {
				return ctrl.Result{}, fmt.Errorf("original PVC name is held by unexpected UID")
			}
			if err := r.Delete(ctx, pvc); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhaseSourcePVCDeleted
	case migrationv1alpha1.VolumeRebindPhaseSourcePVCDeleted:
		storageClass := copy.Spec.TargetStorageClass
		volumeMode := corev1.PersistentVolumeFilesystem
		if targetPV.Spec.VolumeMode != nil {
			volumeMode = *targetPV.Spec.VolumeMode
		}
		accessModes := append([]corev1.PersistentVolumeAccessMode(nil), targetPV.Spec.AccessModes...)
		resources := corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: targetPV.Spec.Capacity[corev1.ResourceStorage]}}
		newPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: copy.Spec.SourcePVC.Name, Namespace: copy.Namespace}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: accessModes, VolumeMode: &volumeMode, StorageClassName: &storageClass, VolumeName: targetPV.Name, Resources: resources}}
		if err := r.Create(ctx, newPVC); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhaseOriginalRecreated
	case migrationv1alpha1.VolumeRebindPhaseOriginalRecreated:
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: copy.Spec.SourcePVC.Name}, pvc); err != nil {
			return ctrl.Result{}, err
		}
		if pvc.Spec.VolumeName != targetPV.Name || pvc.Status.Phase != corev1.ClaimBound {
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		copy.Status.RebindPhase = migrationv1alpha1.VolumeRebindPhaseRebound
	case migrationv1alpha1.VolumeRebindPhaseRebound:
		copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseValidating
	}
	return ctrl.Result{}, nil
}

func (r *VolumeCopyReconciler) patchConsumers(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (ctrl.Result, error) {
	for i := range copy.Status.OriginalWorkloadState {
		state := &copy.Status.OriginalWorkloadState[i]
		changed := false
		switch state.Kind {
		case workloadDeployment:
			obj := &appsv1.Deployment{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: state.Namespace, Name: state.Name}, obj); err != nil {
				return ctrl.Result{}, err
			}
			changed = replaceClaim(obj.Spec.Template.Spec.Volumes, copy.Spec.SourcePVC.Name, copy.Status.TargetPVC.Name)
			if changed {
				constrainToTarget(&obj.Spec.Template.Spec, copy.Status.TargetPlacement.Node)
				if err := r.Update(ctx, obj); err != nil {
					return ctrl.Result{}, err
				}
			}
		case workloadStatefulSet:
			obj := &appsv1.StatefulSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: state.Namespace, Name: state.Name}, obj); err != nil {
				return ctrl.Result{}, err
			}
			changed = replaceClaim(obj.Spec.Template.Spec.Volumes, copy.Spec.SourcePVC.Name, copy.Status.TargetPVC.Name)
			if changed {
				constrainToTarget(&obj.Spec.Template.Spec, copy.Status.TargetPlacement.Node)
				if err := r.Update(ctx, obj); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		if changed {
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
	}
	copy.Status.Phase = migrationv1alpha1.VolumeCopyPhaseValidating
	return ctrl.Result{}, nil
}

func replaceClaim(volumes []corev1.Volume, source, target string) bool {
	changed := false
	for i := range volumes {
		if volumes[i].PersistentVolumeClaim != nil && volumes[i].PersistentVolumeClaim.ClaimName == source {
			volumes[i].PersistentVolumeClaim.ClaimName = target
			changed = true
		}
	}
	return changed
}
func constrainToTarget(spec *corev1.PodSpec, node string) {
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[corev1.LabelHostname] = node
}
func (r *VolumeCopyReconciler) hasVolumeAttachment(ctx context.Context, pv string) (bool, error) {
	list, err := r.KubeClient.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Source.PersistentVolumeName != nil && *list.Items[i].Spec.Source.PersistentVolumeName == pv {
			return true, nil
		}
	}
	return false, nil
}

func (r *VolumeCopyReconciler) cleanupTransferResources(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) error {
	selector := client.MatchingLabels{labelVolumeCopyUID: string(copy.UID)}
	listOptions := []client.ListOption{client.InNamespace(copy.Namespace), selector}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, listOptions...); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := r.Delete(ctx, &pods.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, listOptions...); err != nil {
		return err
	}
	for i := range services.Items {
		if err := r.Delete(ctx, &services.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, listOptions...); err != nil {
		return err
	}
	for i := range secrets.Items {
		if err := r.Delete(ctx, &secrets.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	configMaps := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMaps, listOptions...); err != nil {
		return err
	}
	for i := range configMaps.Items {
		if err := r.Delete(ctx, &configMaps.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := r.List(ctx, serviceAccounts, listOptions...); err != nil {
		return err
	}
	for i := range serviceAccounts.Items {
		if err := r.Delete(ctx, &serviceAccounts.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := r.List(ctx, policies, listOptions...); err != nil {
		return err
	}
	for i := range policies.Items {
		if err := r.Delete(ctx, &policies.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	roles := &rbacv1.RoleList{}
	if err := r.List(ctx, roles, listOptions...); err != nil {
		return err
	}
	for i := range roles.Items {
		if err := r.Delete(ctx, &roles.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	roleBindings := &rbacv1.RoleBindingList{}
	if err := r.List(ctx, roleBindings, listOptions...); err != nil {
		return err
	}
	for i := range roleBindings.Items {
		if err := r.Delete(ctx, &roleBindings.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager registers the resumable VolumeCopy controller.
func (r *VolumeCopyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Engine == nil {
		r.Engine = &PVCTransferEngine{Client: r.Client}
	}
	return ctrl.NewControllerManagedBy(mgr).For(&migrationv1alpha1.VolumeCopy{}).Owns(&corev1.Pod{}).Named("volumecopy").WithOptions(controller.Options{MaxConcurrentReconciles: 2}).Complete(r)
}
