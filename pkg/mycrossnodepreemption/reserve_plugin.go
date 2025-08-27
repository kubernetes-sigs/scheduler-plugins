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
	ap := pl.getActivePlan()
	if ap == nil || ap.PlanDoc.Completed {
		return framework.NewStatus(framework.Success)
	}

	// Pending preemptor (only in every-preemptor mode) doesn't consume workload quota here.
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		klog.V(V2).InfoS("Reserve: pending preemptor; not consuming workload quota", "pod", klog.KObj(pod), "node", ap.PlanDoc.TargetNode)
		return framework.NewStatus(framework.Success)
	}

	wk, ok := topWorkload(pod)
	if !ok {
		klog.V(V2).InfoS("Reserve: pod not part of any workload; allowing", "pod", klog.KObj(pod))
		return framework.NewStatus(framework.Success)
	}

	key := wk.String()
	perNode, ok := ap.PlanDoc.WkDesiredPerNode[key]
	if !ok || perNode[node] == 0 {
		klog.V(V2).InfoS("Reserve: workload not allowed on node", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not allowed on node")
	}

	ctrs, ok := ap.Remaining[key]
	if !ok {
		klog.V(V2).InfoS("Reserve: workload not tracked", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not tracked")
	}
	ctr, ok := ctrs[node]
	if !ok {
		klog.V(V2).InfoS("Reserve: node not tracked", "pod", klog.KObj(pod), "node", node)
		return framework.NewStatus(framework.Unschedulable, "Reserve: node not tracked")
	}

	for {
		cur := ctr.Load()
		if cur <= 0 {
			klog.V(V2).InfoS("Reserve: workload node quota exhausted", "pod", klog.KObj(pod), "node", node)
			return framework.NewStatus(framework.Unschedulable, "Reserve: workload node quota exhausted")
		}
		if ctr.CompareAndSwap(cur, cur-1) {
			klog.V(V2).InfoS("Reserve: workload node quota consumed", "pod", klog.KObj(pod), "node", node)
			st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: key, nodeName: node}})
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
	rsst, ok := v.(*rsReservationState)
	if !ok {
		klog.V(V2).InfoS("Unreserve: failed to cast reservation state", "pod", klog.KObj(pod))
		return
	}

	ap := pl.getActivePlan()
	if ap == nil {
		klog.V(V2).InfoS("Unreserve: no active plan", "pod", klog.KObj(pod))
		return
	}
	if ctrs, ok := ap.Remaining[rsst.key.rsKey]; ok {
		if ctr, ok := ctrs[rsst.key.nodeName]; ok {
			ctr.Add(1)
		}
	}
}
