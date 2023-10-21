package laxity

import (
	"errors"
	"fmt"
	"time"

	gocache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/annotations"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/estimator"
)

const (
	// EstimatorMetricSize describes the size of the metrics expected to be passed to ATLAS LLSP solver.
	// When a metrics passed has more items, it will be truncated.
	// If there are less items, 0 will be added for padding
	EstimatorMetricSize = 5
)

var (
	ErrNotFound = errors.New("pod execution not found in laxity manager")
)

type Manager interface {
	// StartPodExecution starts the pod's execution
	StartPodExecution(pod *v1.Pod)
	// PausePodExecution pause the pod's execution
	PausePodExecution(pod *v1.Pod)
	// GetPodLaxity returns a pod's laxity
	GetPodLaxity(pod *v1.Pod) (time.Duration, error)
	// RemovePodExecution deletes the tracked execution of a pod
	RemovePodExecution(pod *v1.Pod)
}

type laxityManager struct {
	deadlineManager deadline.Manager
	atlas           estimator.Estimator
	podExecutions   *gocache.Cache
}

func NewLaxityManager() Manager {
	return &laxityManager{
		deadlineManager: deadline.NewDeadlineManager(),
		atlas:           estimator.NewATLASEstimator(EstimatorMetricSize),
		podExecutions:   gocache.New(time.Second, time.Second),
	}
}

func getPodExecutionTime(pod *v1.Pod) time.Duration {
	if execTime, err := time.ParseDuration(pod.Annotations[annotations.AnnotationKeyExecTime]); err == nil {
		return execTime
	}
	return 0
}

func getPodMetrics(pod *v1.Pod) estimator.Metrics {
	var metrics estimator.Metrics
	if ddlRel, err := time.ParseDuration(pod.Annotations[annotations.AnnotationKeyDDL]); err == nil {
		metrics = append(metrics, float64(ddlRel))
	}
	metrics = append(metrics, float64(getPodExecutionTime(pod)))
	return metrics
}

func (l *laxityManager) createPodExecutionIfNotExist(pod *v1.Pod) *podExecution {
	key := toCacheKey(pod)
	podExec, ok := l.podExecutions.Get(key)
	if !ok || podExec == nil {
		ddl := l.deadlineManager.GetPodDeadline(pod)
		execTime := getPodExecutionTime(pod)
		metrics := getPodMetrics(pod)
		estExecTime := l.atlas.EstimateExecTime(metrics)
		if estExecTime == 0 {
			l.atlas.Add(metrics, execTime)
			estExecTime = l.atlas.EstimateExecTime(metrics)
		}
		klog.V(5).InfoS("laxityManager: EstimateExecTime", "estExecTime", estExecTime, "execTime", execTime, "metrics", metrics, "pod", klog.KObj(pod))
		podExec = &podExecution{
			deadline:    ddl,
			estExecTime: estExecTime,
		}
		l.podExecutions.Add(key, podExec, -1)
	}
	return podExec.(*podExecution)
}

func (l *laxityManager) StartPodExecution(pod *v1.Pod) {
	podExec := l.createPodExecutionIfNotExist(pod)
	podExec.start()
}

func (l *laxityManager) PausePodExecution(pod *v1.Pod) {
	podExec := l.createPodExecutionIfNotExist(pod)
	podExec.pause()
}

func (l *laxityManager) GetPodLaxity(pod *v1.Pod) (time.Duration, error) {
	podExec := l.createPodExecutionIfNotExist(pod)
	laxity, err := podExec.laxity()
	if err == ErrBeyondEstimation {
		metrics := getPodMetrics(pod)
		klog.V(5).ErrorS(ErrBeyondEstimation, "wrongly estimated execution time, updating estimator...", "metrics", metrics, "actualExecTime", podExec.actualExecTime, "pod", klog.KObj(pod))
		l.atlas.Add(metrics, podExec.actualExecTime)
	}
	return laxity, err
}

func (l *laxityManager) RemovePodExecution(pod *v1.Pod) {
	podExec := l.createPodExecutionIfNotExist(pod)
	podExec.pause()
	metrics := getPodMetrics(pod)
	klog.V(5).InfoS("pod execution finished, updating estimator...", "metrics", metrics, "actualExecTime", podExec.actualExecTime, "pod", klog.KObj(pod))
	l.atlas.Add(metrics, podExec.actualExecTime)
	l.podExecutions.Delete(toCacheKey(pod))
}

func toCacheKey(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
