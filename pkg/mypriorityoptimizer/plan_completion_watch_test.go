// pkg/mypriorityoptimizer/plan_completion_watch_test.go
// plan_completion_watch_test.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// -------------------------
// startPlanCompletionWatch
// -------------------------

func TestStartPlanCompletionWatch_NilActivePlan(t *testing.T) {
	pl := &SharedState{}

	orig := planCompletionWatchFn
	defer func() { planCompletionWatchFn = orig }()

	called := atomic.Bool{}
	planCompletionWatchFn = func(_ *SharedState, _ *ActivePlan) {
		called.Store(true)
	}

	// ap == nil -> should return immediately and not call the watcher.
	pl.startPlanCompletionWatch(nil)

	if called.Load() {
		t.Fatalf("planCompletionWatchFn was called, want not called for nil ap")
	}
}

func TestStartPlanCompletionWatch_SpawnsWatcher(t *testing.T) {
	pl := &SharedState{}
	ctx := context.Background()
	ap := &ActivePlan{
		ID:     "plan-1",
		Ctx:    ctx,
		Cancel: func() {},
	}

	orig := planCompletionWatchFn
	defer func() { planCompletionWatchFn = orig }()

	done := make(chan struct{})
	var gotPL *SharedState
	var gotAP *ActivePlan

	planCompletionWatchFn = func(hpl *SharedState, hap *ActivePlan) {
		gotPL = hpl
		gotAP = hap
		close(done)
	}

	pl.startPlanCompletionWatch(ap)

	select {
	case <-done:
		// ok
	case <-time.After(1 * time.Second):
		t.Fatalf("planCompletionWatchFn was not called from startPlanCompletionWatch")
	}

	if gotPL != pl {
		t.Fatalf("planCompletionWatchFn got pl=%p, want %p", gotPL, pl)
	}
	if gotAP != ap {
		t.Fatalf("planCompletionWatchFn got ap=%p, want %p", gotAP, ap)
	}
}

// -------------------------
// Helpers for planCompletionWatch tests
// -------------------------

// fakeCtx is a small context.Context implementation used to control Done/Err.
type fakeCtx struct {
	done <-chan struct{}
	err  error
}

func (f *fakeCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (f *fakeCtx) Done() <-chan struct{}       { return f.done }
func (f *fakeCtx) Err() error                  { return f.err }
func (f *fakeCtx) Value(key any) any           { return nil }

// -------------------------
// planCompletionWatch – active plan cleared
// -------------------------

func TestPlanCompletionWatch_ActivePlanClearedStopsWatcher(t *testing.T) {
	pl := &SharedState{}

	// Context that never finishes (so only ticker drives the loop).
	ctx := context.Background()
	ap := &ActivePlan{
		ID:     "plan-cleared",
		Ctx:    ctx,
		Cancel: func() {},
	}

	// Make ticker fast for tests.
	origGetInterval := getPlanCompletionCheckInterval
	defer func() { getPlanCompletionCheckInterval = origGetInterval }()
	getPlanCompletionCheckInterval = func() time.Duration {
		return 2 * time.Millisecond
	}

	origGetActive := getActivePlanForWatch
	origIsCompleted := isPlanCompletedFn
	origOnCompleted := onPlanCompletedFn
	defer func() {
		getActivePlanForWatch = origGetActive
		isPlanCompletedFn = origIsCompleted
		onPlanCompletedFn = origOnCompleted
	}()

	// First tick: return nil active plan so watcher exits.
	getActivePlanForWatch = func(_ *SharedState) *ActivePlan {
		return nil
	}
	isPlanCompletedFn = func(_ *SharedState, _ *ActivePlan) (bool, error) {
		t.Fatalf("isPlanCompletedFn should not be called when active plan is nil")
		return false, nil
	}
	onCalled := atomic.Bool{}
	onPlanCompletedFn = func(_ *SharedState, _ PlanStatus) {
		onCalled.Store(true)
	}

	start := time.Now()
	pl.planCompletionWatch(ap)
	elapsed := time.Since(start)

	if onCalled.Load() {
		t.Fatalf("onPlanCompletedFn was called, want not called when active plan is cleared")
	}
	// Sanity check that we didn't block forever.
	if elapsed > time.Second {
		t.Fatalf("planCompletionWatch took too long, watcher may not have stopped (elapsed=%v)", elapsed)
	}
}

// -------------------------
// planCompletionWatch – successful completion path
// -------------------------

func TestPlanCompletionWatch_PlanCompletesSuccessfully(t *testing.T) {
	pl := &SharedState{}

	ctx := context.Background()
	ap := &ActivePlan{
		ID:     "plan-success",
		Ctx:    ctx,
		Cancel: func() {},
	}

	origGetInterval := getPlanCompletionCheckInterval
	defer func() { getPlanCompletionCheckInterval = origGetInterval }()
	getPlanCompletionCheckInterval = func() time.Duration {
		return 2 * time.Millisecond
	}

	origGetActive := getActivePlanForWatch
	origIsCompleted := isPlanCompletedFn
	origOnCompleted := onPlanCompletedFn
	defer func() {
		getActivePlanForWatch = origGetActive
		isPlanCompletedFn = origIsCompleted
		onPlanCompletedFn = origOnCompleted
	}()

	getActivePlanForWatch = func(_ *SharedState) *ActivePlan {
		return ap
	}

	var calls int32
	isPlanCompletedFn = func(_ *SharedState, _ *ActivePlan) (bool, error) {
		// First call: not done, second call: done
		if atomic.AddInt32(&calls, 1) >= 2 {
			return true, nil
		}
		return false, nil
	}

	done := make(chan struct{})
	var gotStatus PlanStatus
	onPlanCompletedFn = func(_ *SharedState, st PlanStatus) {
		gotStatus = st
		close(done)
	}

	go pl.planCompletionWatch(ap)

	select {
	case <-done:
		// ok
	case <-time.After(1 * time.Second):
		t.Fatalf("planCompletionWatch did not mark plan as completed in time")
	}

	if gotStatus != PlanStatusCompleted {
		t.Fatalf("onPlanCompletedFn status = %v, want %v", gotStatus, PlanStatusCompleted)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("isPlanCompletedFn call count = %d, want >= 2", calls)
	}
}

// -------------------------
// planCompletionWatch – timeout (DeadlineExceeded) -> Failed
// -------------------------

func TestPlanCompletionWatch_TimeoutMarksPlanFailed(t *testing.T) {
	pl := &SharedState{}

	doneCh := make(chan struct{})
	// Closed immediately so Done fires right away.
	close(doneCh)

	ctx := &fakeCtx{
		done: doneCh,
		err:  context.DeadlineExceeded,
	}

	ap := &ActivePlan{
		ID:     "plan-timeout",
		Ctx:    ctx,
		Cancel: func() {},
	}

	origGetActive := getActivePlanForWatch
	origOnCompleted := onPlanCompletedFn
	defer func() {
		getActivePlanForWatch = origGetActive
		onPlanCompletedFn = origOnCompleted
	}()

	getActivePlanForWatch = func(_ *SharedState) *ActivePlan {
		return ap
	}

	statusCh := make(chan PlanStatus, 1)
	onPlanCompletedFn = func(_ *SharedState, st PlanStatus) {
		statusCh <- st
	}

	pl.planCompletionWatch(ap)

	select {
	case st := <-statusCh:
		if st != PlanStatusFailed {
			t.Fatalf("onPlanCompletedFn status = %v, want %v", st, PlanStatusFailed)
		}
	default:
		t.Fatalf("onPlanCompletedFn was not called on timeout")
	}
}

// -------------------------
// planCompletionWatch – cancelled context (not DeadlineExceeded) -> no status
// -------------------------

func TestPlanCompletionWatch_CancelledContextDoesNotSettlePlan(t *testing.T) {
	pl := &SharedState{}

	doneCh := make(chan struct{})
	close(doneCh)

	ctx := &fakeCtx{
		done: doneCh,
		err:  context.Canceled,
	}

	ap := &ActivePlan{
		ID:     "plan-cancelled",
		Ctx:    ctx,
		Cancel: func() {},
	}

	origOnCompleted := onPlanCompletedFn
	defer func() { onPlanCompletedFn = origOnCompleted }()

	called := atomic.Bool{}
	onPlanCompletedFn = func(_ *SharedState, _ PlanStatus) {
		called.Store(true)
	}

	pl.planCompletionWatch(ap)

	if called.Load() {
		t.Fatalf("onPlanCompletedFn was called, want not called for context cancellation")
	}
}
