package storagecopy

import (
	"context"
	"fmt"
	"strings"

	"github.com/migtools/pvc-transfer/endpoint/service"
	"github.com/migtools/pvc-transfer/transfer"
	transferrsync "github.com/migtools/pvc-transfer/transfer/rsync"
	"github.com/migtools/pvc-transfer/transport"
	"github.com/migtools/pvc-transfer/transport/stunnel"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

const (
	labelMigrationUID        = "migration.openshift.io/migration-uid"
	labelVolumeCopyUID       = "migration.openshift.io/volume-copy-uid"
	labelComponent           = "migration.openshift.io/transfer-component"
	labelAttempt             = "migration.openshift.io/transfer-attempt"
	transferPort       int32 = 6443
)

// AttemptStatus describes the observable state of one stable transfer pod.
type AttemptStatus struct {
	Exists     bool
	Running    bool
	Completed  bool
	Succeeded  bool
	ExitCode   int32
	StartedAt  *metav1.Time
	FinishedAt *metav1.Time
	PodName    string
}

// PVCTransferEngine creates a private ClusterIP and stunnel-backed rsync transfer.
type PVCTransferEngine struct {
	Client client.Client
}

// EnsureAttempt idempotently creates a target server and read-only source client.
func (e *PVCTransferEngine) EnsureAttempt(ctx context.Context, copy *migrationv1alpha1.VolumeCopy,
	sourcePVC, targetPVC *corev1.PersistentVolumeClaim, sourceNode, targetNode string,
	final bool) (bool, error) {
	if sourceNode == "" || targetNode == "" {
		return false, fmt.Errorf("source and target nodes must be positively selected")
	}
	labels := transferLabels(copy, "receiver")
	owners := []metav1.OwnerReference{*metav1.NewControllerRef(copy, migrationv1alpha1.GroupVersion.WithKind("VolumeCopy"))}
	endpointName := stableResourceName("vcopy", copy.Name)
	logger := ctrl.LoggerFrom(ctx)

	ep, err := service.New(ctx, e.Client, logger, types.NamespacedName{Namespace: copy.Namespace, Name: endpointName},
		transferPort, transferPort, corev1.ServiceTypeClusterIP, labels, nil, owners)
	if err != nil {
		return false, fmt.Errorf("creating private transfer endpoint: %w", err)
	}
	healthy, err := ep.IsHealthy(ctx, e.Client)
	if err != nil || !healthy {
		return false, err
	}

	serverTransport, err := stunnel.NewServer(ctx, e.Client, logger,
		types.NamespacedName{Namespace: copy.Namespace, Name: endpointName}, ep,
		&transport.Options{Labels: labels, Owners: owners, Image: copy.Spec.Transfer.Image})
	if err != nil {
		return false, fmt.Errorf("creating server transport: %w", err)
	}
	terminate := false
	server, err := transferrsync.NewServer(ctx, e.Client, logger, transfer.NewSingletonPVC(targetPVC),
		serverTransport, ep, labels, owners, transfer.PodOptions{
			NodeName: targetNode, Image: copy.Spec.Transfer.Image, TerminateOnCompletion: &terminate,
		})
	if err != nil {
		return false, fmt.Errorf("creating rsync receiver: %w", err)
	}
	serverHealthy, err := server.IsHealthy(ctx, e.Client)
	if err != nil || !serverHealthy {
		return false, nil
	}

	if err := e.ensureNetworkPolicy(ctx, copy); err != nil {
		return false, err
	}
	if err := e.ensureSourcePod(ctx, copy, sourcePVC, sourceNode, ep.Hostname(), ep.IngressPort(), serverTransport.Credentials(), final); err != nil {
		return false, err
	}
	return true, nil
}

func (e *PVCTransferEngine) ensureSourcePod(ctx context.Context, copy *migrationv1alpha1.VolumeCopy,
	sourcePVC *corev1.PersistentVolumeClaim, sourceNode, hostname string, port int32,
	credentials types.NamespacedName, final bool) error {
	labels := transferLabels(copy, "sender")
	labels[labelAttempt] = fmt.Sprintf("%d", copy.Status.Attempt)
	owners := []metav1.OwnerReference{*metav1.NewControllerRef(copy, migrationv1alpha1.GroupVersion.WithKind("VolumeCopy"))}
	name := attemptPodName(copy)
	logger := ctrl.LoggerFrom(ctx)
	clientTransport, err := stunnel.NewClient(ctx, e.Client, logger,
		types.NamespacedName{Namespace: copy.Namespace, Name: name}, hostname, port,
		&transport.Options{Labels: labels, Owners: owners, Image: copy.Spec.Transfer.Image,
			Credentials: &transport.Credentials{SecretRef: credentials, Type: stunnel.CredentialsTypeSSL}})
	if err != nil {
		return fmt.Errorf("creating client transport: %w", err)
	}
	opts, err := BuildRsyncOptions(copy.Spec.Fidelity, final, copy.Spec.Transfer.BandwidthLimit)
	if err != nil {
		return err
	}
	args, err := opts.Options()
	if err != nil {
		return fmt.Errorf("rendering rsync options: %w", err)
	}
	command := append([]string{"/usr/bin/rsync"}, args...)
	command = append(command, "/mnt/source/", fmt.Sprintf("rsync://root@localhost/data/ --port %d", clientTransport.ListenPort()))
	script := fmt.Sprintf("set -o pipefail; %s; rc=$?; sync; touch /usr/share/rsync/done; exit $rc", strings.Join(command, " "))

	transportContainers := clientTransport.Containers()
	for i := range transportContainers {
		transportContainers[i].Image = copy.Spec.Transfer.Image
		transportContainers[i].VolumeMounts = append(transportContainers[i].VolumeMounts,
			corev1.VolumeMount{Name: "communication", MountPath: "/usr/share/rsync"})
		transportContainers[i].Command = []string{"/bin/bash", "-c",
			"/bin/stunnel /etc/stunnel/stunnel.conf; while [ ! -f /usr/share/rsync/done ]; do sleep 1; done"}
	}
	containers := []corev1.Container{{
		Name: "rsync", Image: copy.Spec.Transfer.Image,
		Command: []string{"/bin/bash", "-c", script},
		VolumeMounts: []corev1.VolumeMount{{Name: "source", MountPath: "/mnt/source", ReadOnly: true},
			{Name: "communication", MountPath: "/usr/share/rsync"}},
	}}
	containers = append(containers, transportContainers...)
	volumes := []corev1.Volume{
		{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sourcePVC.Name, ReadOnly: true}}},
		{Name: "communication", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
	volumes = append(volumes, clientTransport.Volumes()...)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: copy.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, e.Client, pod, func() error {
		pod.Labels = labels
		pod.OwnerReferences = owners
		if pod.CreationTimestamp.IsZero() {
			pod.Spec = corev1.PodSpec{NodeName: sourceNode, RestartPolicy: corev1.RestartPolicyNever,
				SchedulerName: copy.Spec.Transfer.SchedulerName, Containers: containers, Volumes: volumes}
		}
		return nil
	})
	return err
}

// Status observes the stable source transfer pod without parsing logs.
func (e *PVCTransferEngine) Status(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) (AttemptStatus, error) {
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, client.ObjectKey{Namespace: copy.Namespace, Name: attemptPodName(copy)}, pod)
	if apierrors.IsNotFound(err) {
		return AttemptStatus{}, nil
	}
	if err != nil {
		return AttemptStatus{}, fmt.Errorf("getting transfer pod: %w", err)
	}
	status := AttemptStatus{Exists: true, PodName: pod.Name}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "rsync" {
			continue
		}
		if cs.State.Running != nil {
			status.Running = true
			status.StartedAt = &cs.State.Running.StartedAt
		}
		if cs.State.Terminated != nil {
			status.Completed = true
			status.Succeeded = cs.State.Terminated.ExitCode == 0
			status.ExitCode = cs.State.Terminated.ExitCode
			status.StartedAt = &cs.State.Terminated.StartedAt
			status.FinishedAt = &cs.State.Terminated.FinishedAt
		}
	}
	return status, nil
}

func (e *PVCTransferEngine) ensureNetworkPolicy(ctx context.Context, copy *migrationv1alpha1.VolumeCopy) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: stableResourceName("vcopy", copy.Name), Namespace: copy.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, e.Client, policy, func() error {
		policy.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(copy, migrationv1alpha1.GroupVersion.WithKind("VolumeCopy"))}
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: transferLabels(copy, "receiver")},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: transferLabels(copy, "sender")}}},
			}},
		}
		return nil
	})
	return err
}

func transferLabels(copy *migrationv1alpha1.VolumeCopy, component string) map[string]string {
	return map[string]string{labelMigrationUID: string(copy.Spec.MigrationUID), labelVolumeCopyUID: string(copy.UID), labelComponent: component}
}

func stableResourceName(prefix, name string) string {
	value := prefix + "-" + name
	if len(value) > 63 {
		return value[:63]
	}
	return strings.TrimSuffix(value, "-")
}

func attemptPodName(copy *migrationv1alpha1.VolumeCopy) string {
	return stableResourceName("vcopy", fmt.Sprintf("%s-%d", copy.Name, copy.Status.Attempt))
}
