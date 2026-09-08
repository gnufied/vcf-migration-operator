package storagecopy

import (
	"testing"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestResolveNodePlacementsUsesMachineServer(t *testing.T) {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid", Labels: map[string]string{regionLabel: "region", zoneLabel: "zone"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	machine := machinev1beta1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker-machine"}, Spec: machinev1beta1.MachineSpec{ProviderSpec: machinev1beta1.ProviderSpec{Value: &runtime.RawExtension{Raw: []byte("{\"workspace\":{\"server\":\"target.example.com\"}}")}}}, Status: machinev1beta1.MachineStatus{NodeRef: &corev1.ObjectReference{Name: "worker"}}}
	placements, err := ResolveNodePlacements([]corev1.Node{node}, []machinev1beta1.Machine{machine})
	if err != nil {
		t.Fatalf("ResolveNodePlacements: %v", err)
	}
	if len(placements) != 1 || placements[0].VCenter != "target.example.com" {
		t.Fatalf("unexpected placements: %#v", placements)
	}
}

func TestValidateTargetStorageClass(t *testing.T) {
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "target"}, Provisioner: VSphereCSIDriverName, VolumeBindingMode: &wffc, AllowedTopologies: []corev1.TopologySelectorTerm{{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: regionLabel, Values: []string{"region"}}, {Key: zoneLabel, Values: []string{"zone"}}}}}}
	if err := ValidateTargetStorageClass(sc, "region", "zone"); err != nil {
		t.Fatalf("ValidateTargetStorageClass: %v", err)
	}
	sc.AllowedTopologies = nil
	if err := ValidateTargetStorageClass(sc, "region", "zone"); err == nil {
		t.Fatal("unconstrained StorageClass accepted")
	}
}

func TestValidateTargetStorageClassRejectsUnsafeTerms(t *testing.T) {
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	tests := map[string][]corev1.TopologySelectorTerm{
		"split terms": {
			{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: regionLabel, Values: []string{"region"}}}},
			{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{{Key: zoneLabel, Values: []string{"zone"}}}},
		},
		"broad zone": {{MatchLabelExpressions: []corev1.TopologySelectorLabelRequirement{
			{Key: regionLabel, Values: []string{"region"}},
			{Key: zoneLabel, Values: []string{"zone", "source-zone"}},
		}}},
	}
	for name, terms := range tests {
		t.Run(name, func(t *testing.T) {
			sc := &storagev1.StorageClass{
				Provisioner:       VSphereCSIDriverName,
				VolumeBindingMode: &wffc,
				AllowedTopologies: terms,
			}
			if err := ValidateTargetStorageClass(sc, "region", "zone"); err == nil {
				t.Fatal("unsafe target topology accepted")
			}
		})
	}
}
