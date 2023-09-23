package util

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
)

func MakePod(podName string, namespace string, memReq int64, cpuReq int64, ddl string, uid string, nodeName string, createdAt *time.Time, phase v1.PodPhase) *v1.Pod {
	now := time.Now()
	if createdAt == nil {
		createdAt = &now
	}
	pause := imageutils.GetPauseImageName()
	pod := st.MakePod().Namespace(namespace).Name(podName).Container(pause).
		Node(nodeName).UID(uid).ZeroTerminationGracePeriod().UID(podName).
		CreationTimestamp(metav1.NewTime(*createdAt)).Phase(phase).
		Annotations(map[string]string{deadline.AnnotationKeyDDL: ddl}).Obj()
	pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(memReq, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(cpuReq, resource.DecimalSI),
		},
	}
	return pod
}
