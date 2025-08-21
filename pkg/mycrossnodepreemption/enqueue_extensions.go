// enqueue_extensions.go

package mycrossnodepreemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

var _ framework.EnqueueExtensions = &MyCrossNodePreemption{}

// After a plan completes or is deleted, we want to requeue pods that were blocked by it.
func (pl *MyCrossNodePreemption) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{
			Event: framework.ClusterEvent{
				Resource:   framework.Pod,
				ActionType: framework.All,
			},
			QueueingHintFn: func(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
				if pod == nil {
					return framework.QueueSkip, nil
				}
				sp, _ := pl.getActivePlan()

				// If plan is gone and we had blocked this pod → requeue it.
				if (sp == nil || sp.Completed) && pl.blocked.has(pod.UID) {
					return framework.Queue, nil
				}
				// While plan is active (or pod wasn't one we blocked), skip.
				return framework.QueueSkip, nil
			},
		},
	}, nil
}
