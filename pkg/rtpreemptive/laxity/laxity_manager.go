package laxity

import (
	"errors"
	"fmt"
	"time"

	gocache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/annotations"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/estimator"
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

func NewLaxityManager(deadlineManager deadline.Manager) Manager {
	return &laxityManager{
		deadlineManager: deadlineManager,
		atlas:           estimator.NewATLASEstimator(),
		podExecutions:   gocache.New(time.Second, time.Second),
	}
}

func (l *laxityManager) createPodExecutionIfNotExist(pod *v1.Pod) *podExecution {
	key := toCacheKey(pod)
	podExec, ok := l.podExecutions.Get(key)
	if !ok || podExec == nil {
		ddl := l.deadlineManager.GetPodDeadline(pod)
		// TODO: pass appropriate metrics to estimator
		metrics := make(map[string]interface{})
		metrics["ddl"], _ = time.ParseDuration(pod.Annotations[annotations.AnnotationKeyDDL])
		metrics["exec_time"], _ = time.ParseDuration(pod.Annotations[annotations.AnnotationKeyExecTime])
		// ENDTODO
		estExecTime := l.atlas.EstimateExecTime(metrics)
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
	return podExec.laxity()
}

func (l *laxityManager) RemovePodExecution(pod *v1.Pod) {
	l.podExecutions.Delete(toCacheKey(pod))
}

func toCacheKey(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
