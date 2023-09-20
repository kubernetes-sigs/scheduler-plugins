package preemption

import (
	"errors"
	"fmt"
	"time"

	gocache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
)

const (
	// AnnotationKeyPrefix is the prefix of the annotation key
	AnnotationKeyPrefix = "rt-preemptive.scheduling.x-k8s.io/"
	// AnnotationKeyPausePod represents whether or not a pod is marked to be paused
	AnnotationKeyPausePod = AnnotationKeyPrefix + "pause-pod"
)

var (
	ErrPodNotFound = errors.New("pod not found in cache")
)

type Manager interface {
	IsPodMarkedPaused(pod *v1.Pod) bool
	AddPausedPod(pod *v1.Pod, node *v1.Node)
	RemovePausedPod(pod *v1.Pod)
	GetPausedPodNode(pod *v1.Pod) (*v1.Node, error)
}

// PreemptionManager maintains information related to current paused pods
type preemptionManager struct {
	pausedPods *gocache.Cache
	nodeLister corelisters.NodeLister
}

func NewPreemptionManager(nodeLister corelisters.NodeLister) Manager {
	return &preemptionManager{
		pausedPods: gocache.New(time.Second*5, time.Second*5),
		nodeLister: nodeLister,
	}
}

func (m *preemptionManager) IsPodMarkedPaused(pod *v1.Pod) bool {
	val, ok := pod.Annotations[AnnotationKeyPausePod]
	if !ok {
		return false
	}
	return val == "true"
}

func (m *preemptionManager) AddPausedPod(pod *v1.Pod, node *v1.Node) {
	m.pausedPods.Set(string(pod.UID), node.Name, time.Hour)
}

func (m *preemptionManager) RemovePausedPod(pod *v1.Pod) {
	m.pausedPods.Delete(string(pod.UID))
	m.pausedPods.DeleteExpired()
}

func (m *preemptionManager) GetPausedPodNode(pod *v1.Pod) (*v1.Node, error) {
	nodeName, ok := m.pausedPods.Get(string(pod.UID))
	if !ok {
		return nil, ErrPodNotFound
	}
	node, err := m.nodeLister.Get(nodeName.(string))
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return node, nil
}
