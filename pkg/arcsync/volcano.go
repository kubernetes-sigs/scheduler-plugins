package arcsync

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const queueAnnotationKey = "scheduling.volcano.sh/queue-name"

func getQueueNpuLimit(pod *v1.Pod, qLister queueLister, fullResourceName v1.ResourceName) (int64, bool) {
	if pod == nil || qLister == nil {
		return 0, false
	}
	queueName := pod.Annotations[queueAnnotationKey]
	if queueName == "" {
		return 0, false
	}
	obj, found, err := qLister.Get(queueName)
	if !found || err != nil {
		return 0, false
	}
	capability, found, err := unstructured.NestedMap(obj.Object, "spec", "capability")
	if !found || err != nil {
		return 0, false
	}
	val, exists := capability[string(fullResourceName)]
	if !exists {
		return 0, false
	}
	strVal, ok := val.(string)
	if !ok {
		strVal = fmt.Sprintf("%v", val)
	}
	q, err := resource.ParseQuantity(strVal)
	if err != nil {
		return 0, false
	}
	return q.Value(), true
}
