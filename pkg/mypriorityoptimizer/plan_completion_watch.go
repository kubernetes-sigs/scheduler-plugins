// pkg/mypriorityoptimizer/plan_completion_watch.go
// plan_completion_watch.go
package mypriorityoptimizer

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// Small hooks for easier testing.
var planCompletionWatchFn = func(pl *SharedState, ap *ActivePlan) {
	pl.planCompletionWatch(ap)
}

var getActivePlanForWatch = func(pl *SharedState) *ActivePlan {
	return pl.getActivePlan()
}

var isPlanCompletedFn = func(pl *SharedState, ap *ActivePlan) (bool, error) {
	return pl.isPlanCompleted(ap)
}

var onPlanCompletedFn = func(pl *SharedState, status PlanStatus) {
	pl.onPlanCompleted(status)
}

var getPlanCompletionCheckInterval = func() time.Duration {
	return PlanCompletionCheckInterval
}

// startPlanCompletionWatch spawns a goroutine that periodically checks
// whether the active plan has been realized, or has timed out.
// It will call onPlanCompleted(Completed/Failed) at most once (guarded inside
// onPlanCompleted), and then exit.
func (pl *SharedState) startPlanCompletionWatch(ap *ActivePlan) {
	if ap == nil {
		klog.V(MyV).Info("startPlanCompletionWatch: no active plan provided")
		return
	}
	go planCompletionWatchFn(pl, ap)
}

// planCompletionWatch loops until the plan is either completed (success),
// times out (PlanExecutionTimeout), or is replaced/cleared.
func (pl *SharedState) planCompletionWatch(ap *ActivePlan) {
	label := "PlanCompletionWatch"
	interval := getPlanCompletionCheckInterval()
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	klog.InfoS(msg(label, "started"),
		"planID", ap.ID,
		"interval", interval,
		"timeout", PlanExecutionTimeout,
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ap.Ctx.Done():
			// Either PlanExecutionTimeout or explicit Cancel() invoked.
			err := ap.Ctx.Err()
			if err == context.DeadlineExceeded {
				cur := getActivePlanForWatch(pl)
				if cur != nil && cur.ID == ap.ID {
					klog.InfoS(msg(label, "plan timed out; settling as failed"),
						"planID", ap.ID,
						"timeout", PlanExecutionTimeout,
					)
					onPlanCompletedFn(pl, PlanStatusFailed)
				}
			} else {
				klog.V(MyV).InfoS(msg(label, "plan context cancelled; exiting watcher"),
					"planID", ap.ID,
					"ctxErr", err,
				)
			}
			return

		case <-ticker.C:
			// If active plan changed or was cleared, stop watching.
			cur := getActivePlanForWatch(pl)
			if cur == nil || cur.ID != ap.ID {
				klog.V(MyV).InfoS(msg(label, "active plan changed or cleared; stopping"),
					"planID", ap.ID,
				)
				return
			}

			done, err := isPlanCompletedFn(pl, cur)
			if err != nil {
				// Lister errors etc. – just log and retry.
				klog.V(MyV).InfoS(msg(label, "isPlanCompleted error; will retry"),
					"planID", ap.ID,
					"err", err.Error(),
				)
				continue
			}
			if !done {
				continue
			}

			// Plan has been realized successfully: mark completed.
			klog.InfoS(msg(label, "plan completed; settling"),
				"planID", ap.ID,
			)
			onPlanCompletedFn(pl, PlanStatusCompleted)
			return
		}
	}
}
