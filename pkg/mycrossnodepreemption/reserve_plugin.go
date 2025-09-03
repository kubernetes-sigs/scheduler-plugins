// reserve_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const rsReservationKey framework.StateKey = "myx/rsReservation"

type reservationKey struct {
	rsKey    string
	nodeName string
}

type rsReservationState struct {
	key reservationKey
}

func (s *rsReservationState) Clone() framework.StateData {
	return &rsReservationState{
		key: s.key,
	}
}

// Reserve is called at the end of scheduling cycle to reserve resources for a pod on a specific node.
// If it fails, the Unreserve function is called to release any reserved resources.
// It is used, here, to place workload pods on the appropriate nodes as they are automatically created, therefore, placement by name cannot be done.
func (pl *MyCrossNodePreemption) Reserve(ctx context.Context, st *framework.CycleState, pod *v1.Pod, node string) *framework.Status {
	// Don't reserve kube-system pods
	if pod.Namespace == "kube-system" {
		return framework.NewStatus(framework.Success)
	}
	if !pl.IsActivePlan() {
		return framework.NewStatus(framework.Success)
	}
	ap := pl.getActivePlan()

	// Always let preemptor pods through; not part of workload quotas
	if ap.PlanDoc.Preemptor != nil && string(pod.UID) == ap.PlanDoc.Preemptor.Pod.UID {
		return framework.NewStatus(framework.Success)
	}

	// Check if pod is part of a workload; if not, allow it immediately.
	// Otherwise, we need to check the workload quota.
	wk, ok := topWorkload(pod)
	if !ok {
		klog.V(V2).InfoS("Reserve: pod not part of any workload; allowing", "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)
	}
	workloadKey := wk.String()
	desiredPerNode, ok := ap.PlanDoc.WorkloadPerNode[workloadKey]
	if !ok || desiredPerNode[node] == 0 {
		klog.V(V2).InfoS("Reserve: workload not allowed on node", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not allowed on node")
	}
	allWorkloadCnts, ok := ap.WorkloadPerNodeCnts[workloadKey]
	if !ok {
		klog.V(V2).InfoS("Reserve: workload not tracked", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not tracked")
	}
	workloadCntForNode, ok := allWorkloadCnts[node]
	if !ok {
		klog.V(V2).InfoS("Reserve: node not tracked", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: node not tracked")
	}

	for {
		currentCnt := workloadCntForNode.Load()
		if currentCnt <= 0 {
			klog.V(V2).InfoS("Reserve: workload node quota exhausted", "pod", klog.KObj(pod), "node", node)
			return framework.NewStatus(framework.Unschedulable, "Reserve: workload node quota exhausted")
		}
		if workloadCntForNode.CompareAndSwap(currentCnt, currentCnt-1) { // successfully reserved quota
			klog.V(V2).InfoS("Reserve: workload node quota consumed", "pod", klog.KObj(pod), "node", node)
			st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: workloadKey, nodeName: node}})
			return framework.NewStatus(framework.Success)
		}
	}
}

func (pl *MyCrossNodePreemption) Unreserve(ctx context.Context, st *framework.CycleState, pod *v1.Pod, _ string) {
	v, err := st.Read(rsReservationKey)
	if err != nil {
		klog.V(V2).InfoS("Unreserve: failed to read reservation state", "pod", klog.KObj(pod))
		return
	}
	reservationState, ok := v.(*rsReservationState)
	if !ok {
		klog.V(V2).InfoS("Unreserve: failed to cast reservation state", "pod", klog.KObj(pod))
		return
	}

	ap := pl.getActivePlan()
	if ap == nil {
		klog.V(V2).InfoS("Unreserve: no active plan", "pod", klog.KObj(pod))
		return
	}
	if allWorkloadCnts, ok := ap.WorkloadPerNodeCnts[reservationState.key.rsKey]; ok {
		if ctr, ok := allWorkloadCnts[reservationState.key.nodeName]; ok {
			ctr.Add(1) // return quota
		}
	}
}
