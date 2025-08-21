// postbind_plugin.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind runs after a pod is bound by the scheduler.
// When the preemptor binds and our plan goals are satisfied,
// we mark the plan completed and clear the "blocked" set.
// Requeueing of previously blocked pods is then driven by our
// QueueingHintFn registered on Pod/ConfigMap events.
func (pl *MyCrossNodePreemption) PostBind(
	_ context.Context,
	_ *framework.CycleState,
	_ *v1.Pod,
	_ string,
) {
	sp, cmName := pl.getActivePlan()
	if sp == nil || sp.Completed {
		return
	}

	ok, err := pl.isPlanCompleted(context.TODO(), sp)
	if err != nil {
		klog.ErrorS(err, "PostBind: completion check failed")
		return
	}
	if !ok {
		klog.V(2).InfoS("PostBind: plan still active")
		return
	}

	// Mark completed both in-memory and in the persisted plan doc.
	pl.clearActivePlan()
	pl.markPlanCompleted(context.TODO(), cmName)

	// Snapshot blocked pods (UID -> {ns,name}) and nudge each by patching an annotation.
	if pl.blocked != nil {
		for uid, bi := range pl.blocked.snapshot() {
			if err := pl.patchPodByName(bi.ns, bi.name); err != nil {
				klog.ErrorS(err, "PostBind: failed to nudge blocked pod", "uid", string(uid), "pod", bi.ns+"/"+bi.name)
			}
		}
		pl.blocked.clear()
	}

	klog.InfoS("PostBind: plan completed; cleared active plan and nudged blocked pods")
}

// patchPodByName emits a no-op annotation update for the given Pod.
// This triggers a Pod Update event without changing scheduling semantics.
func (pl *MyCrossNodePreemption) patchPodByName(ns, name string) error {
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{"crossnode-plan/wakeup":"%d"}}}`,
		time.Now().UnixNano(),
	))
	_, err := pl.client.CoreV1().Pods(ns).Patch(
		context.TODO(),
		name,
		apitypes.StrategicMergePatchType,
		patch,
		metav1.PatchOptions{FieldManager: "my-crossnode-plugin"},
	)
	return err
}
