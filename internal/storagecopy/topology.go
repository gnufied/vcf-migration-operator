package storagecopy

import (
	"encoding/json"
	"fmt"
	"sort"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

const (
	regionLabel = "topology.kubernetes.io/region"
	zoneLabel   = "topology.kubernetes.io/zone"
)

// NodePlacement is a node identity positively mapped through its Machine provider spec.
type NodePlacement struct {
	NodeName    string
	NodeUID     string
	Machine     string
	VCenter     string
	Region      string
	Zone        string
	Ready       bool
	Schedulable bool
}

type machineProviderSpec struct {
	Workspace struct {
		Server string `json:"server"`
	} `json:"workspace"`
}

// ResolveNodePlacements maps Nodes to Machines and vCenters without trusting topology labels alone.
func ResolveNodePlacements(nodes []corev1.Node, machines []machinev1beta1.Machine) ([]NodePlacement, error) {
	machinesByNode := make(map[string]*machinev1beta1.Machine, len(machines))
	for i := range machines {
		machine := &machines[i]
		if machine.Status.NodeRef == nil || machine.Status.NodeRef.Name == "" {
			continue
		}
		if existing := machinesByNode[machine.Status.NodeRef.Name]; existing != nil {
			return nil, fmt.Errorf("node %q maps to multiple Machines %q and %q", machine.Status.NodeRef.Name, existing.Name, machine.Name)
		}
		machinesByNode[machine.Status.NodeRef.Name] = machine
	}

	placements := make([]NodePlacement, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		machine := machinesByNode[node.Name]
		if machine == nil || machine.Spec.ProviderSpec.Value == nil {
			continue
		}
		var provider machineProviderSpec
		if err := json.Unmarshal(machine.Spec.ProviderSpec.Value.Raw, &provider); err != nil {
			return nil, fmt.Errorf("decoding provider spec for Machine %s: %w", machine.Name, err)
		}
		if provider.Workspace.Server == "" {
			return nil, fmt.Errorf("machine %s provider spec has no workspace.server", machine.Name)
		}
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		placements = append(placements, NodePlacement{
			NodeName: node.Name, NodeUID: string(node.UID), Machine: machine.Name,
			VCenter: provider.Workspace.Server, Region: node.Labels[regionLabel], Zone: node.Labels[zoneLabel],
			Ready: ready, Schedulable: !node.Spec.Unschedulable,
		})
	}
	sort.Slice(placements, func(i, j int) bool { return placements[i].NodeName < placements[j].NodeName })
	return placements, nil
}

// SelectTargetNode returns a Ready, schedulable node mapped to the target vCenter and topology.
func SelectTargetNode(placements []NodePlacement, vCenter, region, zone string) (*NodePlacement, error) {
	for i := range placements {
		p := &placements[i]
		if p.Ready && p.Schedulable && p.VCenter == vCenter && p.Region == region && p.Zone == zone {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no Ready schedulable node maps to target vCenter %q in region %q zone %q", vCenter, region, zone)
}

// ValidateTargetStorageClass rejects topology that can provision into an unintended site.
func ValidateTargetStorageClass(sc *storagev1.StorageClass, region, zone string) error {
	if sc == nil {
		return fmt.Errorf("target StorageClass must not be nil")
	}
	if sc.Provisioner != VSphereCSIDriverName {
		return fmt.Errorf("target StorageClass %q uses provisioner %q, want %q", sc.Name, sc.Provisioner, VSphereCSIDriverName)
	}
	if !allowedTopologiesSelectOnly(sc.AllowedTopologies, region, zone) {
		return fmt.Errorf("target StorageClass %q allowedTopologies must constrain every term to only region %q and zone %q", sc.Name, region, zone)
	}
	if sc.VolumeBindingMode == nil {
		return fmt.Errorf("target StorageClass %q has no volumeBindingMode", sc.Name)
	}
	switch *sc.VolumeBindingMode {
	case storagev1.VolumeBindingWaitForFirstConsumer, storagev1.VolumeBindingImmediate:
		return nil
	default:
		return fmt.Errorf("target StorageClass %q has unsupported volumeBindingMode %q", sc.Name, *sc.VolumeBindingMode)
	}
}

func allowedTopologiesSelectOnly(terms []corev1.TopologySelectorTerm, region, zone string) bool {
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		regionMatched := false
		zoneMatched := false
		for _, expression := range term.MatchLabelExpressions {
			switch expression.Key {
			case regionLabel:
				regionMatched = len(expression.Values) == 1 && expression.Values[0] == region
			case zoneLabel:
				zoneMatched = len(expression.Values) == 1 && expression.Values[0] == zone
			}
		}
		if !regionMatched || !zoneMatched {
			return false
		}
	}
	return true
}
