// hook_postfilter_test.go

package mypriorityoptimizer

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

func TestPostFilter(t *testing.T) {
	// common: avoid 1s sleep
	origSleep := postFilterSleep
	postFilterSleep = func(time.Duration) {}
	t.Cleanup(func() { postFilterSleep = origSleep })

	type tc struct {
		name          string
		perPodEnabled bool
		setupPL       func(pl *SharedState)
		runOpt        func(pl *SharedState, ctx context.Context, p *v1.Pod) (*Plan, error)

		wantCode     fwk.Code
		wantMsgSub   string
		wantNomNode  string
		wantBlocked  int
		wantResIsNil bool
	}

	tests := []tc{
		{
			name:          "no per-pod -> no nomination",
			perPodEnabled: false,
			runOpt: func(_ *SharedState, _ context.Context, _ *v1.Pod) (*Plan, error) {
				t.Fatalf("run optimization should not be called")
				return nil, nil
			},
			wantCode:     fwk.Unschedulable,
			wantMsgSub:   "PostFilter: no nomination",
			wantResIsNil: true,
		},
		{
			name:          "active plan -> blocks pod",
			perPodEnabled: true,
			setupPL: func(pl *SharedState) {
				pl.ActivePlan.Store(&ActivePlan{ID: "ap1", PlacementByName: map[string]string{}})
			},
			runOpt: func(_ *SharedState, _ context.Context, _ *v1.Pod) (*Plan, error) {
				t.Fatalf("run optimization should not be called when active plan exists")
				return nil, nil
			},
			wantCode:     fwk.Unschedulable,
			wantMsgSub:   "PostFilter: " + InfoActivePlanInProgress,
			wantBlocked:  1,
			wantResIsNil: true,
		},
		{
			name:          "optimization returns ErrActiveInProgress -> blocks pod",
			perPodEnabled: true,
			runOpt: func(_ *SharedState, _ context.Context, _ *v1.Pod) (*Plan, error) {
				return nil, ErrActiveInProgress
			},
			wantCode:     fwk.Unschedulable,
			wantMsgSub:   "PostFilter: " + InfoActivePlanInProgress,
			wantBlocked:  1,
			wantResIsNil: true,
		},
		{
			name:          "optimization returns generic error -> plan registration failed",
			perPodEnabled: true,
			runOpt: func(_ *SharedState, _ context.Context, _ *v1.Pod) (*Plan, error) {
				return nil, context.Canceled
			},
			wantCode:     fwk.Unschedulable,
			wantMsgSub:   "PostFilter: " + InfoPlanRegistrationFailed,
			wantBlocked:  0,
			wantResIsNil: true,
		},
		{
			name:          "success -> returns nomination",
			perPodEnabled: true,
			runOpt: func(_ *SharedState, _ context.Context, _ *v1.Pod) (*Plan, error) {
				return &Plan{NominatedNode: "nodeA"}, nil
			},
			wantCode:    fwk.Success,
			wantMsgSub:  "PostFilter: " + InfoNominatedAfterPlan,
			wantNomNode: "nodeA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// stub seams per test
			origPerPod := postFilterPerPodEnabled
			origRun := postFilterRunOptimization
			postFilterPerPodEnabled = func() bool { return tt.perPodEnabled }
			postFilterRunOptimization = tt.runOpt
			t.Cleanup(func() {
				postFilterPerPodEnabled = origPerPod
				postFilterRunOptimization = origRun
			})

			pl := &SharedState{BlockedWhileActive: newSafePodSet("blocked")}
			if tt.setupPL != nil {
				tt.setupPL(pl)
			}

			res, st := pl.PostFilter(context.Background(), nil, makePod("default", "p1", "", "", "", "", 0), nil)

			if tt.wantResIsNil && res != nil {
				t.Fatalf("PostFilter() result = %#v, want nil", res)
			}
			mustHookStatus(t, "PostFilter", st, tt.wantCode, tt.wantMsgSub)

			if tt.wantNomNode != "" {
				if res == nil || res.NominatingInfo == nil {
					t.Fatalf("PostFilter() result = %#v, want NominatingInfo", res)
				}
				if res.NominatingInfo.NominatedNodeName != tt.wantNomNode {
					t.Fatalf("NominatedNodeName = %q, want %q", res.NominatingInfo.NominatedNodeName, tt.wantNomNode)
				}
			}

			if tt.wantBlocked != 0 || pl.BlockedWhileActive != nil {
				if got := pl.BlockedWhileActive.Size(); got != tt.wantBlocked {
					t.Fatalf("BlockedWhileActive.Size() = %d, want %d", got, tt.wantBlocked)
				}
			}
		})
	}
}
