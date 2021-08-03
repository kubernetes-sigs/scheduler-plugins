package volumetopologyspread

import (
	"context"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Based on https://kubernetes.io/docs/concepts/storage/storage-classes/#volume-binding-mode
type VolumeTopologySpread struct {
	handle framework.Handle
}

// var _ = framework.FilterPlugin(&VolumeTopologySpread{})

const (
	Name = "VolumeTopologySpread"
	// https://kubernetes.io/docs/reference/labels-annotations-taints/#topologykubernetesiozone
	zoneTopologyKey = "topology.kubernetes.io/zone"
)

func (v *VolumeTopologySpread) Name() string {
	return Name
}

func (v *VolumeTopologySpread) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	labelSelector := getConstraintLabelSelector(pod)
	if labelSelector == nil {
		// This pod does not have a hard topology constraint on zone
		return framework.NewStatus(framework.Success)
	}

	nodeZone, ok := nodeInfo.Node().Labels[zoneTopologyKey]
	if !ok {
		// This node is missing the zone topology key
		return framework.NewStatus(framework.Success)
	}

	// Assumption: whatever label selector is provided for the pod topology constraint must also be present on the pvc.
	// Ideally it would be present on the volume but stateful sets don't support labeling the volumes
	names, err := v.getVolumeNames(pod.Namespace, *labelSelector)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable)
	}

	zones, err := v.getVolumeZones(names)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable)
	}

	if _, exists := zones[nodeZone]; !exists {
		return framework.NewStatus(framework.Success)
	}

	return framework.NewStatus(framework.UnschedulableAndUnresolvable)
}

func (v *VolumeTopologySpread) getVolumeZones(volumesNames []string) (map[string]struct{}, error) {
	pvLister := v.handle.SharedInformerFactory().Core().V1().PersistentVolumes().Lister()
	zones := map[string]struct{}{}
	for _, name := range volumesNames {
		pv, err := pvLister.Get(name)
		if err != nil {
			return nil, err
		}
		zone, ok := pv.Labels[zoneTopologyKey]
		if !ok {
			continue
		}
		zones[zone] = struct{}{}
	}
	return zones, nil
}

func (v *VolumeTopologySpread) getVolumeNames(namespace string, selector labels.Selector) ([]string, error) {
	claims, err := v.handle.SharedInformerFactory().Core().V1().PersistentVolumeClaims().Lister().PersistentVolumeClaims(namespace).List(selector)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(claims))
	for i, claim := range claims {
		names[i] = claim.Spec.VolumeName
	}
	return names, nil
}

// Only enforce a hard constraint keyed on zones
func getConstraintLabelSelector(pod *v1.Pod) *labels.Selector {
	for _, constraint := range pod.Spec.TopologySpreadConstraints {
		if constraint.WhenUnsatisfiable == v1.DoNotSchedule {
			if constraint.TopologyKey == zoneTopologyKey {
				if constraint.LabelSelector == nil {
					return nil
				}
				var requirements labels.Requirements
				for k, v := range constraint.LabelSelector.MatchLabels {
					req, err := labels.NewRequirement(k, selection.In, []string{v})
					if err != nil {
						continue
					}
					if req == nil {
						continue
					}
					requirements = append(requirements, *req)
				}
				selector := labels.NewSelector().Add(requirements...)
				return &selector
			}
		}
	}
	return nil
}

func getClaimNames(pod *v1.Pod) []string {
	var claims []string
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			claims = append(claims, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	return claims
}
