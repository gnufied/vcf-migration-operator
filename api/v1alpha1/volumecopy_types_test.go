package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestVolumeCopyScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	for _, kind := range []string{"VolumeCopy", "VolumeCopyList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Fatalf("Scheme.New(%s): %v", kind, err)
		}
	}
}

func TestVolumeCopyDeepCopy(t *testing.T) {
	orig := &VolumeCopyList{
		Items: []VolumeCopy{{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"site": "source"}},
			Spec: VolumeCopySpec{
				SourcePVC:             MigrationObjectReference{Name: "data", UID: "pvc-uid"},
				TargetFailureDomain:   "target-a",
				DataMigrationStrategy: VolumeDataMigrationStrategyRsync,
			},
			Status: VolumeCopyStatus{Phase: VolumeCopyPhasePlanned},
		}},
	}
	copied := orig.DeepCopyObject().(*VolumeCopyList)
	copied.Items[0].Labels["site"] = "target"
	copied.Items[0].Spec.SourcePVC.UID = "replacement-uid"
	copied.Items[0].Spec.TargetFailureDomain = "target-b"
	copied.Items[0].Status.Phase = VolumeCopyPhaseCopied
	got := orig.Items[0]
	if got.Labels["site"] != "source" || got.Spec.SourcePVC.UID != "pvc-uid" ||
		got.Spec.TargetFailureDomain != "target-a" ||
		got.Status.Phase != VolumeCopyPhasePlanned {
		t.Fatalf("DeepCopyObject: mutating the copy changed the original: %+v", got)
	}
}
