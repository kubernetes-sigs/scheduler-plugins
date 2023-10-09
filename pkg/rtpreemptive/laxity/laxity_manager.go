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
	ErrOverdue          = errors.New("already passed deadline")
	ErrBeyondEstimation = errors.New("actual execution time is beyond initial estimation")
	ErrNotFound         = errors.New("pod execution not found in laxity manager")
)

type podExecution struct {
	deadline       time.Time
	estExecTime    time.Duration
	actualExecTime time.Duration
}

func (p *podExecution) laxity() (time.Duration, error) {
	now := time.Now()
	timeToDDL := p.deadline.Sub(now)
	if timeToDDL < 0 {
		return 0, ErrOverdue
	}
	remainingExecTime := p.estExecTime - p.actualExecTime
	if remainingExecTime < 0 {
		return 0, ErrBeyondEstimation
	}
	return timeToDDL - remainingExecTime, nil
}

type Manager interface {
	// GetPodLaxity returns a pod's laxity
	GetPodLaxity(pod *v1.Pod) (time.Duration, error)
	// IncrPodExecution adds the given execTime to tracked actual execution time of a pod
	IncrPodExecution(pod *v1.Pod, execTime time.Duration) error
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

func (l *laxityManager) GetPodLaxity(pod *v1.Pod) (time.Duration, error) {
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
	return podExec.(*podExecution).laxity()
}

func (l *laxityManager) IncrPodExecution(pod *v1.Pod, execTime time.Duration) error {
	key := toCacheKey(pod)
	podExec, ok := l.podExecutions.Get(key)
	if !ok || podExec == nil {
		return ErrNotFound
	}
	podExec.(*podExecution).actualExecTime += execTime
	return nil
}

func (l *laxityManager) RemovePodExecution(pod *v1.Pod) {
	l.podExecutions.Delete(toCacheKey(pod))
}

func toCacheKey(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}
