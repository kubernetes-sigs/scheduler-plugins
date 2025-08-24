// reserve_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
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
	ap := pl.getActive()
	if ap == nil || ap.PlanDoc.Completed {
		return framework.NewStatus(framework.Success)
	}

	// Pending preemptor (only in every-preemptor mode) doesn't consume workload quota here.
	if ap.PlanDoc.TargetNode != "" && string(pod.UID) == ap.PlanDoc.PendingUID {
		return framework.NewStatus(framework.Success)
	}

	wk, ok := topWorkload(pod)
	if !ok {
		return framework.NewStatus(framework.Success)
	}

	key := wk.String()
	perNode, ok := ap.PlanDoc.WkDesiredPerNode[key]
	if !ok || perNode[node] == 0 {
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not allowed on node")
	}

	ctrs, ok := ap.Remaining[key]
	if !ok {
		return framework.NewStatus(framework.Unschedulable, "Reserve: workload not tracked")
	}
	ctr, ok := ctrs[node]
	if !ok {
		return framework.NewStatus(framework.Unschedulable, "Reserve: node not tracked")
	}

	for {
		cur := ctr.Load()
		if cur <= 0 {
			return framework.NewStatus(framework.Unschedulable, "Reserve: workload node quota exhausted")
		}
		if ctr.CompareAndSwap(cur, cur-1) {
			st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: key, nodeName: node}})
			return framework.NewStatus(framework.Success)
		}
	}
}

func (pl *MyCrossNodePreemption) Unreserve(ctx context.Context, st *framework.CycleState, pod *v1.Pod, _ string) {
	v, err := st.Read(rsReservationKey)
	if err != nil {
		return
	}
	rsst, ok := v.(*rsReservationState)
	if !ok {
		return
	}

	ap := pl.getActive()
	if ap == nil {
		return
	}
	if ctrs, ok := ap.Remaining[rsst.key.rsKey]; ok {
		if ctr, ok := ctrs[rsst.key.nodeName]; ok {
			ctr.Add(1)
		}
	}
}
