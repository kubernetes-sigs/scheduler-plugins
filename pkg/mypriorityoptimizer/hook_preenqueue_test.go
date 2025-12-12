// hook_preenqueue_test.go
package mypriorityoptimizer

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

func TestPreEnqueue(t *testing.T) {
	type tc struct {
		name string

		// inputs
		pod          *v1.Pod
		pluginReady  bool
		mode         ModeType
		synch        bool
		activePlan   *ActivePlan
		placementMap map[string]string // convenience: ActivePlan.PlacementByName

		// expected
		wantCode    fwk.Code
		wantBlocked int
	}

	tests := []tc{
		{
			name:        "kube-system always allowed",
			pod:         makePod(SystemNamespace, "sys-pod", "", "", "", "", 0),
			pluginReady: false, // doesn't matter
			mode:        ModeManualBlocking,
			synch:       true,
			wantCode:    fwk.Success,
			wantBlocked: 0,
		},
		{
			name:        "caches not ready blocks pod",
			pod:         makePod("default", "p1", "", "", "", "", 0),
			pluginReady: false,
			mode:        ModePeriodic,
			synch:       true,
			wantCode:    fwk.Pending,
			wantBlocked: 1,
		},
		{
			name:        "manual blocking mode blocks when no active plan",
			pod:         makePod("default", "work-pod", "", "", "", "", 0),
			pluginReady: true,
			mode:        ModeManualBlocking,
			synch:       true,
			wantCode:    fwk.Pending,
			wantBlocked: 0,
		},
		{
			name:        "default-like mode pass-through when no active plan",
			pod:         makePod("default", "work-pod", "", "", "", "", 0),
			pluginReady: true,
			mode:        ModePeriodic,
			synch:       true,
			wantCode:    fwk.Success,
			wantBlocked: 0,
		},
		{
			name:        "active plan blocks pod not allowed by plan",
			pod:         makePod("default", "p1", "", "", "", "", 0),
			pluginReady: true,
			mode:        ModePeriodic,
			synch:       true,
			activePlan:  &ActivePlan{ID: "ap1"},
			// PlacementByName empty => p1 not allowed
			wantCode:    fwk.Pending,
			wantBlocked: 1,
		},
		{
			name:        "active plan allows pinned pod",
			pod:         makePod("default", "p1", "", "", "", "", 0),
			pluginReady: true,
			mode:        ModePeriodic,
			synch:       true,
			activePlan:  &ActivePlan{ID: "ap1"},
			placementMap: map[string]string{
				"default/p1": "node1",
			},
			wantCode:    fwk.Success,
			wantBlocked: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
			pl.PluginReady.Store(tt.pluginReady)

			if tt.activePlan != nil {
				ap := *tt.activePlan
				if tt.placementMap != nil {
					ap.PlacementByName = tt.placementMap
				} else if ap.PlacementByName == nil {
					ap.PlacementByName = map[string]string{}
				}
				pl.ActivePlan.Store(&ap)
			}

			withMode(tt.mode, tt.synch, func() {
				st := pl.PreEnqueue(context.Background(), tt.pod)
				mustHookStatus(t, "PreEnqueue", st, tt.wantCode, "")

				if pl.BlockedWhileActive != nil {
					if got := pl.BlockedWhileActive.Size(); got != tt.wantBlocked {
						t.Fatalf("BlockedWhileActive.Size() = %d, want %d", got, tt.wantBlocked)
					}
				}
			})
		})
	}
}
