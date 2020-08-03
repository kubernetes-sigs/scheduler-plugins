package fixednode

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	appslister "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
)

const (
	Name               = "FixedNodeScheduling"
	ownerResource      = "StatefulSet"
	fixedNodeEnableKey = "fixednode.scheduling.sigs.k8s.io"
	fixedNodeRecordKey = "fixednode.scheduling.sigs.k8s.io/record"
	reasonDisable      = "fixednode scheduling not enable"
)

var (
	_ framework.FilterPlugin   = &FixedNode{}
	_ framework.PostBindPlugin = &FixedNode{}
)

// FixedNode a scheduler plugin for statefulSet pod
type FixedNode struct {
	frameworkHandle   framework.FrameworkHandle
	statefulSetLister appslister.StatefulSetLister
	clientSet         kubernetes.Interface
}

func newScheduleRecords(sts *appsv1.StatefulSet) map[string]string {
	var record = make(map[string]string)
	if v, ok := sts.Annotations[fixedNodeRecordKey]; ok {
		// ignore this error
		_ = json.Unmarshal([]byte(v), &record)
	}

	return record
}

// Name implement from framework.Plugin
func (fn *FixedNode) Name() string {
	return Name
}

func (fn *FixedNode) enableFixedNodeScheduling(pod *corev1.Pod) bool {
	if v, ok := pod.Labels[fixedNodeEnableKey]; ok && v == "true" {
		return true
	}
	return false
}

func (fn *FixedNode) setScheduledRecord(ctx context.Context, sts *appsv1.StatefulSet, scheduledRecords map[string]string) error {
	stsCopy := sts.DeepCopy()

	data, err := json.Marshal(scheduledRecords)
	if err != nil {
		return err
	}

	if stsCopy.Annotations == nil {
		stsCopy.Annotations = make(map[string]string)
	}

	stsCopy.Annotations[fixedNodeRecordKey] = string(data)

	_, err = fn.clientSet.AppsV1().StatefulSets(stsCopy.Namespace).Update(ctx, stsCopy, metav1.UpdateOptions{})
	if err != nil {
		err = fmt.Errorf("sts do update err: %w", err)
	}
	return err
}
func (fn *FixedNode) getStatefulSet(pod *corev1.Pod) (*appsv1.StatefulSet, error) {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == ownerResource {
			return fn.statefulSetLister.StatefulSets(pod.Namespace).Get(owner.Name)
		}
	}

	return nil, fmt.Errorf("not found  pod %s/%s owner statefulSet", pod.Namespace, pod.Name)
}

// Filter implement from framework.FilterPlugin
func (fn *FixedNode) Filter(_ context.Context, _ *framework.CycleState, pod *corev1.Pod, nodeInfo *schedulernodeinfo.NodeInfo) *framework.Status {
	// 1. check whether the pod labels contains fixednode scheduling label
	if !fn.enableFixedNodeScheduling(pod) {
		return framework.NewStatus(framework.Success, reasonDisable)
	}

	// 2. get sts from pod owner
	sts, err := fn.getStatefulSet(pod)
	if err != nil {
		reason := fmt.Sprintf("get stateful set err: %v", err)
		klog.V(3).Info(reason)
		return framework.NewStatus(framework.Error, reason)
	}

	scheduledRecords := newScheduleRecords(sts)

	// 3. judge node info is equal to the last scheduled node
	if nodeName, ok := scheduledRecords[pod.Name]; ok {
		if nodeInfo.Node().Name != nodeName {
			reason := fmt.Sprintf("fixednode scheduled %s/%s to %s,so this pod can't assign to %s", pod.Namespace, pod.Name, nodeName, nodeInfo.Node().Name)
			klog.V(3).Info(reason)
			return framework.NewStatus(framework.Unschedulable, reason)
		}
	}

	return framework.NewStatus(framework.Success)
}

// PostBind implement from frame.PostBindPlugin
func (fn *FixedNode) PostBind(ctx context.Context, _ *framework.CycleState, pod *corev1.Pod, nodeName string) {
	if !fn.enableFixedNodeScheduling(pod) {
		return
	}

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		sts, err := fn.getStatefulSet(pod)
		if err != nil {
			klog.Errorf("get stateful set err: %v", err)
			return err
		}

		scheduledRecords := newScheduleRecords(sts)

		if _, ok := scheduledRecords[pod.Name]; !ok {
			scheduledRecords[pod.Name] = nodeName
			return fn.setScheduledRecord(ctx, sts, scheduledRecords)
		}

		return nil
	})

	if err != nil {
		klog.Errorf("update pod %s/%s owner statefulSet fixednode scheduled record err: %v", pod.Namespace, pod.Name, err)
	}
}

// New initializes a new plugin and returns it.
func New(_ *runtime.Unknown, h framework.FrameworkHandle) (framework.Plugin, error) {
	return &FixedNode{
		frameworkHandle:   h,
		statefulSetLister: h.SharedInformerFactory().Apps().V1().StatefulSets().Lister(),
		clientSet:         h.ClientSet(),
	}, nil
}
