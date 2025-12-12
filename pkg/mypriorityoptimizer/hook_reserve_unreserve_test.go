// hook_reserve_unreserve_test.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// -------------------------
// Helpers
// -------------------------

func ai32(v int32) *atomic.Int32 {
	a := new(atomic.Int32)
	a.Store(v)
	return a
}

// -------------------------
// Reserve
// -------------------------

func TestReserve(t *testing.T) {
	type tc struct {
		name         string
		setup        func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string)
		wantCode     fwk.Code
		wantMsgSub   string
		wantStateKey *reservationKey // if non-nil, state must be written
	}

	tests := []tc{
		{
			name: "kube-system always allowed (ignores plan/quota)",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas:  WorkloadQuotasAtomics{},
				})
				pod := makePod(SystemNamespace, "sys-pod", "", "", "", "", 0)
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode: fwk.Success,
		},
		{
			name: "no active plan allows pod",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pl := &SharedState{}
				pod := makePod("default", "work-pod", "", "", "", "", 0)
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode: fwk.Success,
		},
		{
			name: "placementByName pass-through",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID: "ap1",
					PlacementByName: map[string]string{
						"default/p1": "node1",
					},
					WorkloadQuotas: WorkloadQuotasAtomics{}, // should not be touched
				})
				pod := makePod("default", "p1", "", "", "", "", 0)
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode: fwk.Success,
		},
		{
			name: "pod not in workload allows pod",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas:  WorkloadQuotasAtomics{},
				})
				pod := makePod("default", "p1", "", "", "", "", 0)
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode: fwk.Success,
		},
		{
			name: "workload not tracked -> Unschedulable",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas:  WorkloadQuotasAtomics{}, // missing workload key
				})
				pod := makePod("default", "p1", "uid1", "", "ReplicaSet", "rs1", 0)
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode:   fwk.Unschedulable,
			wantMsgSub: "workload not tracked",
		},
		{
			name: "node not tracked -> Unschedulable",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pod := makePod("default", "p1", "uid1", "", "ReplicaSet", "rs1", 0)
				wk, ok := getTopWorkload(pod)
				if !ok {
					t.Fatalf("expected workload pod")
				}
				wkKey := wk.String()

				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas: WorkloadQuotasAtomics{
						wkKey: {"node2": ai32(1)},
					},
				})
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode:   fwk.Unschedulable,
			wantMsgSub: "node not tracked",
		},
		{
			name: "workload node quota exhausted -> Unschedulable",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pod := makePod("default", "p1", "uid1", "", "ReplicaSet", "rs1", 0)
				wk, ok := getTopWorkload(pod)
				if !ok {
					t.Fatalf("expected workload pod")
				}
				wkKey := wk.String()

				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas: WorkloadQuotasAtomics{
						wkKey: {"node1": ai32(0)},
					},
				})
				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode:   fwk.Unschedulable,
			wantMsgSub: "workload node quota exhausted",
		},
		{
			name: "consumes quota and writes reservation state",
			setup: func(t *testing.T) (*SharedState, *v1.Pod, *framework.CycleState, string) {
				pod := makePod("default", "p1", "uid1", "", "ReplicaSet", "rs1", 0)
				wk, ok := getTopWorkload(pod)
				if !ok {
					t.Fatalf("expected workload pod")
				}
				wkKey := wk.String()

				c := ai32(1)

				pl := &SharedState{}
				pl.ActivePlan.Store(&ActivePlan{
					ID:              "ap1",
					PlacementByName: map[string]string{},
					WorkloadQuotas: WorkloadQuotasAtomics{
						wkKey: {"node1": c},
					},
				})

				return pl, pod, framework.NewCycleState(), "node1"
			},
			wantCode:     fwk.Success,
			wantStateKey: &reservationKey{rsKey: "rs:default/rs1", nodeName: "node1"}, // overwritten in-loop below after we compute wkKey
			// quota assertion will be done by reading the exact counter from ActivePlan (below)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl, pod, cs, node := tt.setup(t)

			st := pl.Reserve(context.Background(), cs, pod, node)
			mustHookStatus(t, "Reserve", st, tt.wantCode, tt.wantMsgSub)

			// If this case is the "consumes quota" case, assert quota + state using the real wkKey.
			if tt.name == "consumes quota and writes reservation state" {
				wk, ok := getTopWorkload(pod)
				if !ok {
					t.Fatalf("expected workload pod")
				}
				wkKey := wk.String()

				ap := pl.getActivePlan()
				ctr := ap.WorkloadQuotas[wkKey][node]
				if ctr == nil {
					t.Fatalf("expected quota counter for wkKey=%q node=%q", wkKey, node)
				}
				if got := ctr.Load(); got != 0 {
					t.Fatalf("quota after Reserve = %d, want 0", got)
				}

				data, err := cs.Read(rsReservationKey)
				if err != nil {
					t.Fatalf("CycleState.Read(rsReservationKey) err = %v", err)
				}
				rs, ok := data.(*rsReservationState)
				if !ok {
					t.Fatalf("reservation state type = %T, want *rsReservationState", data)
				}
				want := reservationKey{rsKey: wkKey, nodeName: node}
				if rs.key != want {
					t.Fatalf("reservation key = %#v, want %#v", rs.key, want)
				}
			}

			// Other cases: if caller asked for a reservation key, enforce it (rare; currently only the consume case uses it)
			if tt.wantStateKey != nil && tt.name != "consumes quota and writes reservation state" {
				data, err := cs.Read(rsReservationKey)
				if err != nil {
					t.Fatalf("CycleState.Read(rsReservationKey) err = %v", err)
				}
				rs, ok := data.(*rsReservationState)
				if !ok {
					t.Fatalf("reservation state type = %T, want *rsReservationState", data)
				}
				if rs.key != *tt.wantStateKey {
					t.Fatalf("reservation key = %#v, want %#v", rs.key, *tt.wantStateKey)
				}
			}
		})
	}
}

// -------------------------
// Unreserve
// -------------------------

type badState struct{}

func (b *badState) Clone() fwk.StateData { return &badState{} }

func TestUnreserve(t *testing.T) {
	type tc struct {
		name string

		// inputs
		cycleWrite fwk.StateData // written under rsReservationKey; nil => don't write
		activePlan *ActivePlan

		// optional quota assertion
		wantQuota bool
		wkKey     string
		node      string
		start     int32
		want      int32
	}

	tests := []tc{
		{
			name:       "no reservation state -> no panic / no-op",
			cycleWrite: nil,
			activePlan: nil,
		},
		{
			name:       "reservation state wrong type -> no panic / no-op",
			cycleWrite: &badState{},
			activePlan: nil,
		},
		{
			name:       "reservation state present but no active plan -> no panic / no-op",
			cycleWrite: &rsReservationState{key: reservationKey{rsKey: "wk/ns/foo", nodeName: "node1"}},
			activePlan: nil,
		},
		{
			name:       "active plan present returns quota",
			cycleWrite: &rsReservationState{key: reservationKey{rsKey: "rs:default/rs1", nodeName: "node1"}},
			activePlan: func() *ActivePlan {
				c := ai32(0)
				return &ActivePlan{
					ID: "ap1",
					WorkloadQuotas: WorkloadQuotasAtomics{
						"rs:default/rs1": {"node1": c},
					},
					PlacementByName: map[string]string{},
				}
			}(),
			wantQuota: true,
			wkKey:     "rs:default/rs1",
			node:      "node1",
			start:     0,
			want:      1,
		},
		{
			name:       "active plan present but workload/node missing -> no panic / no-op",
			cycleWrite: &rsReservationState{key: reservationKey{rsKey: "missing", nodeName: "node1"}},
			activePlan: func() *ActivePlan {
				return &ActivePlan{
					ID:              "ap1",
					WorkloadQuotas:  WorkloadQuotasAtomics{"other": {"node2": ai32(5)}},
					PlacementByName: map[string]string{},
				}
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := &SharedState{}
			if tt.activePlan != nil {
				pl.ActivePlan.Store(tt.activePlan)
			}

			cs := framework.NewCycleState()
			if tt.cycleWrite != nil {
				cs.Write(rsReservationKey, tt.cycleWrite)
			}

			pod := makePod("default", "p1", "", "", "", "", 0)
			pl.Unreserve(context.Background(), cs, pod, "node1")

			if tt.wantQuota {
				ap := pl.getActivePlan()
				ctr := ap.WorkloadQuotas[tt.wkKey][tt.node]
				if ctr == nil {
					t.Fatalf("expected quota counter for %q/%q", tt.wkKey, tt.node)
				}
				if got := ctr.Load(); got != tt.want {
					t.Fatalf("quota after Unreserve = %d, want %d", got, tt.want)
				}
			}
		})
	}
}

// -------------------------
// rsReservationState.Clone
// -------------------------

func TestRsReservationStateClone(t *testing.T) {
	orig := &rsReservationState{
		key: reservationKey{
			rsKey:    "wk/ns/foo",
			nodeName: "node1",
		},
	}
	clone := orig.Clone().(*rsReservationState)

	if clone == orig {
		t.Fatalf("Clone() returned same pointer, want distinct instance")
	}
	if clone.key != orig.key {
		t.Fatalf("Clone() key = %#v, want %#v", clone.key, orig.key)
	}
}
