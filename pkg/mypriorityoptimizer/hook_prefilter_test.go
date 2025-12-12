// hook_prefilter_test.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func mustNodeNames(t *testing.T, res *framework.PreFilterResult, want []string) {
	t.Helper()
	if len(want) == 0 {
		if res != nil {
			t.Fatalf("PreFilter() result = %#v, want nil", res)
		}
		return
	}
	if res == nil || res.NodeNames == nil {
		t.Fatalf("PreFilter() result = %#v, want NodeNames", res)
	}
	for _, n := range want {
		if !res.NodeNames.Has(n) {
			t.Fatalf("PreFilter() NodeNames = %#v, want to contain %q", res.NodeNames.UnsortedList(), n)
		}
	}
	// ensure no extras if caller expects an exact set
	if got, wantLen := res.NodeNames.Len(), len(want); got != wantLen {
		t.Fatalf("PreFilter() NodeNames = %#v, want exactly %#v", res.NodeNames.UnsortedList(), want)
	}
}

func TestPreFilterExtensions_IsNil(t *testing.T) {
	pl := &SharedState{}
	if ext := pl.PreFilterExtensions(); ext != nil {
		t.Fatalf("PreFilterExtensions() = %#v, want nil", ext)
	}
}

func TestPreFilter(t *testing.T) {
	type tc struct {
		name string

		pod        *v1.Pod
		activePlan *ActivePlan

		// optional plan setup for quotas case
		setup func(pl *SharedState) *v1.Pod

		wantCode    fwk.Code
		wantNodes   []string // nil => res must be nil
		wantBlocked int
	}

	tests := []tc{
		{
			name:        "kube-system always allowed (ignores plan)",
			pod:         makePod(SystemNamespace, "sys-pod", "", "", "", "", 0),
			activePlan:  &ActivePlan{ID: "ap1", PlacementByName: map[string]string{}},
			wantCode:    fwk.Success,
			wantNodes:   nil,
			wantBlocked: 0,
		},
		{
			name:        "no active plan allows pod",
			pod:         makePod("default", "work-pod", "", "", "", "", 0),
			activePlan:  nil,
			wantCode:    fwk.Success,
			wantNodes:   nil,
			wantBlocked: 0,
		},
		{
			name:       "active plan: standalone allowed on all nodes",
			pod:        makePod("default", "p1", "", "", "", "", 0),
			activePlan: &ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": ""}},
			wantCode:   fwk.Success,
			wantNodes:  nil, // nil => allowed everywhere returns nil result
		},
		{
			name:       "active plan: standalone pinned returns node set",
			pod:        makePod("default", "p1", "", "", "", "", 0),
			activePlan: &ActivePlan{ID: "ap1", PlacementByName: map[string]string{"default/p1": "nodeA"}},
			wantCode:   fwk.Success,
			wantNodes:  []string{"nodeA"},
		},
		{
			name: "active plan: workload quota allows subset of nodes",
			setup: func(pl *SharedState) *v1.Pod {
				prio := int32(0)
				pod := makePod("default", "p1", "uid1", "", "ReplicaSet", "rs1", prio)
				wk, ok := getTopWorkload(pod)
				if !ok {
					t.Fatalf("expected workload pod")
				}

				var c1, c0 atomic.Int32
				c1.Store(1)
				c0.Store(0)

				pl.ActivePlan.Store(&ActivePlan{
					ID: "ap1",
					WorkloadQuotas: WorkloadQuotasAtomics{
						wk.String(): {
							"nodeA": &c1,
							"nodeB": &c0,
						},
					},
					PlacementByName: map[string]string{},
				})
				return pod
			},
			wantCode:  fwk.Success,
			wantNodes: []string{"nodeA"},
		},
		{
			name:        "active plan blocks pod not in plan",
			pod:         makePod("default", "p1", "", "", "", "", 0),
			activePlan:  &ActivePlan{ID: "ap1", PlacementByName: map[string]string{}},
			wantCode:    fwk.Unschedulable,
			wantNodes:   nil,
			wantBlocked: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}

			// default plan injection
			if tt.activePlan != nil {
				pl.ActivePlan.Store(tt.activePlan)
			}

			pod := tt.pod
			if tt.setup != nil {
				pod = tt.setup(pl)
			}

			res, st := pl.PreFilter(context.Background(), framework.NewCycleState(), pod, nil)
			mustHookStatus(t, "PreFilter", st, tt.wantCode, "")

			if tt.wantCode == fwk.Success {
				// nil => allowed everywhere (or kube-system/no plan)
				mustNodeNames(t, res, tt.wantNodes)
			} else {
				// on block, result must be nil
				if res != nil {
					t.Fatalf("PreFilter() result = %#v, want nil when blocking", res)
				}
			}

			if got := pl.BlockedWhileActive.Size(); got != tt.wantBlocked {
				t.Fatalf("BlockedWhileActive.Size() = %d, want %d", got, tt.wantBlocked)
			}
		})
	}
}
