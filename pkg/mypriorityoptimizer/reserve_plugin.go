// reserve_plugin.go

package mypriorityoptimizer

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Reserve is called at the end of scheduling cycle to reserve resources for a pod on a specific node.
// If it fails, the Unreserve function is called to release any reserved resources.
// It is used, here, to place workload pods on the appropriate nodes as they are automatically created, therefore, placement by name cannot be done.
func (pl *MyPriorityOptimizer) Reserve(ctx context.Context, st *framework.CycleState, pending *v1.Pod, node string) *framework.Status {

	stage := "Reserve"

	ap := pl.getActivePlan()
	if pending.Namespace == SystemNamespace || ap == nil {
		return framework.NewStatus(framework.Success)
	}

	// Pass through placementByName pods; do not consume workload quota.
	if ap.PlacementByName != nil {
		if _, ok := ap.PlacementByName[combineNsName(pending.Namespace, pending.Name)]; ok {
			klog.V(MyV).InfoS(msg(stage, InfoPodPlacedByName), "pod", klog.KObj(pending))
			return framework.NewStatus(framework.Success)
		}
	}

	// Check if pod is part of a workload; if not, allow it immediately.
	// Otherwise, we need to check the workload quota.
	wk, ok := topWorkload(pending)
	if !ok {
		klog.V(MyV).InfoS(msg(stage, "pod not part of any workload; allowing"), "pod", klog.KObj(pending))
		return framework.NewStatus(framework.Success)
	}
	// Check if workload is tracked in the active plan.
	workloadKey := wk.String()
	allWorkloadCnts, ok := ap.WorkloadPerNodeCnts[workloadKey]
	if !ok {
		klog.V(MyV).InfoS(msg(stage, "workload not tracked"), "pod", klog.KObj(pending), "node", node)
		return framework.NewStatus(framework.Unschedulable, msg(stage, "workload not tracked"))
	}
	// Check if node is tracked for this workload.
	workloadCntForNode, ok := allWorkloadCnts[node]
	if !ok {
		klog.V(MyV).InfoS(msg(stage, "node not tracked"), "pod", klog.KObj(pending), "node", node)
		return framework.NewStatus(framework.Unschedulable, msg(stage, "node not tracked"))
	}

	// Try to consume workload quota for this pod on this node.
	// We continue to try until we succeed or the quota is exhausted.
	for {
		currentCnt := workloadCntForNode.Load()
		if currentCnt <= 0 {
			klog.V(MyV).InfoS(msg(stage, "workload node quota exhausted"), "pod", klog.KObj(pending), "node", node)
			return framework.NewStatus(framework.Unschedulable, msg(stage, "workload node quota exhausted"))
		}
		if workloadCntForNode.CompareAndSwap(currentCnt, currentCnt-1) { // successfully reserved quota
			klog.V(MyV).InfoS(msg(stage, "workload node quota consumed"), "pod", klog.KObj(pending), "node", node)
			st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: workloadKey, nodeName: node}})
			return framework.NewStatus(framework.Success)
		}
	}
}

func (pl *MyPriorityOptimizer) Unreserve(ctx context.Context, st *framework.CycleState, pending *v1.Pod, _ string) {

	stage := "Unreserve"

	// Read reservation state
	stateData, err := st.Read(rsReservationKey)
	if err != nil {
		klog.V(MyV).InfoS(msg(stage, "failed to read reservation state"), "pod", klog.KObj(pending))
		return
	}
	// Get reservation info
	reservationState, ok := stateData.(*rsReservationState)
	if !ok {
		klog.V(MyV).InfoS(msg(stage, "failed to cast reservation state"), "pod", klog.KObj(pending))
		return
	}

	ap := pl.getActivePlan()
	if ap == nil {
		klog.V(MyV).InfoS(msg(stage, InfoNoActivePlan), "pod", klog.KObj(pending))
		return
	}
	// Return quota
	if allWorkloadCnts, ok := ap.WorkloadPerNodeCnts[reservationState.key.rsKey]; ok {
		if ctr, ok := allWorkloadCnts[reservationState.key.nodeName]; ok {
			ctr.Add(1) // return quota
		}
	}
}

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
