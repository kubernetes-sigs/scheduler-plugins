// reserve_plugin.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type reservationKey struct {
	rsKey    string
	nodeName string
}

type rsReservationState struct {
	key reservationKey
}

// rsReservationKey is used as the key for storing reservation state in CycleState.
const rsReservationKey framework.StateKey = "myx/rsReservation"

// Clone implements framework.StateData interface for rsReservationState.
func (s *rsReservationState) Clone() framework.StateData {
	return &rsReservationState{
		key: s.key,
	}
}

func (pl *MyCrossNodePreemption) Reserve(ctx context.Context, st *framework.CycleState, pod *v1.Pod, node string) *framework.Status {
	sp, planID := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return framework.NewStatus(framework.Success)
	}

	if string(pod.UID) == sp.PendingUID {
		return framework.NewStatus(framework.Success)
	}

	rs, ok := owningRS(pod)

	// If not RS-pod, allow reservation
	if !ok {
		return framework.NewStatus(framework.Success)
	}

	key := rsKey(pod.Namespace, rs)
	perNode, ok := sp.RSDesiredPerNode[key]
	if !ok || perNode[node] == 0 {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS not allowed on node")
	}

	slots := pl.slotsPtr.Load()
	if slots == nil || slots.planID != planID {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: plan changed")
	}
	ctrs, ok := slots.remaining[key]
	if !ok {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS not tracked")
	}
	ctr, ok := ctrs[node]
	if !ok {
		return framework.NewStatus(framework.Unschedulable, "stop-the-world: node not tracked")
	}

	for {
		cur := ctr.Load()
		if cur <= 0 {
			return framework.NewStatus(framework.Unschedulable, "stop-the-world: RS node quota exhausted")
		}
		if ctr.CompareAndSwap(cur, cur-1) {
			st.Write(rsReservationKey, &rsReservationState{key: reservationKey{rsKey: key, nodeName: node}})
			return framework.NewStatus(framework.Success)
		}
	}
}

// If pod reserved a slot but was not scheduled, release the reservation
func (pl *MyCrossNodePreemption) Unreserve(ctx context.Context, st *framework.CycleState, pod *v1.Pod, _ string) {
	v, err := st.Read(rsReservationKey)
	if err != nil {
		return
	}
	rsst, ok := v.(*rsReservationState)
	if !ok {
		return
	}

	slots := pl.slotsPtr.Load()
	if slots == nil {
		return
	}
	ctrs, ok := slots.remaining[rsst.key.rsKey]
	if !ok {
		return
	}
	ctr, ok := ctrs[rsst.key.nodeName]
	if !ok {
		return
	}
	ctr.Add(1) // atomic increment back
}
