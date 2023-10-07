package preemption

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gocache "github.com/patrickmn/go-cache"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/annotations"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/priorityqueue"
)

var (
	ErrPodNotFound        = errors.New("pod not found in cache")
	ErrPodNotBoundToNode  = errors.New("pod is not bound to node")
	ErrPodNotPaused       = errors.New("pod is not paused")
	ErrPodAlreadyPaused   = errors.New("pod is already paused")
	ErrPodCannotBeResumed = errors.New("pod cannot be resumed")
)

type Candidate struct {
	NodeName string
	Pod      *v1.Pod
}

type Manager interface {
	// IsPodMarkedPaused checks if a pod is marked to be paused
	IsPodMarkedPaused(pod *v1.Pod) bool
	// GetPausedCandidateOnNode returns a paused candidate pod if found
	// returns ErrPodNotFound if no candidate found
	GetPausedCandidateOnNode(ctx context.Context, nodeName string) *Candidate
	// ResumeCandidate sets a candidate pod to resumed if it passes the following checks
	// 1. already bound to a node and status paused
	// 2. managed by preemption manager
	// 3. node has enough resources to resume pod
	ResumeCandidate(ctx context.Context, candidate *Candidate) (*Candidate, error)
	// PauseCandidate sets a candidate pod to paused
	PauseCandidate(ctx context.Context, candidate *Candidate) (*Candidate, error)
}

type PriorityFunc func(*v1.Pod) int64

// PreemptionManager maintains information related to current paused pods
type preemptionManager struct {
	pausedPods      *gocache.Cache
	deadlineManager deadline.Manager
	podLister       corelisters.PodLister
	nodeLister      corelisters.NodeLister
	nodeInfoLister  framework.NodeInfoLister
	clientSet       kubernetes.Interface
	priorityFunc    PriorityFunc
}

func NewPreemptionManager(podLister corelisters.PodLister, nodeLister corelisters.NodeLister, nodeInfoLister framework.NodeInfoLister,
	clientSet kubernetes.Interface, priorityFunc PriorityFunc) Manager {
	return &preemptionManager{
		pausedPods:      gocache.New(time.Hour*5, time.Second*5),
		deadlineManager: deadline.NewDeadlineManager(),
		podLister:       podLister,
		nodeLister:      nodeLister,
		nodeInfoLister:  nodeInfoLister,
		clientSet:       clientSet,
		priorityFunc:    priorityFunc,
	}
}

func (m *preemptionManager) IsPodMarkedPaused(pod *v1.Pod) bool {
	val, ok := pod.Annotations[annotations.AnnotationKeyPausePod]
	if !ok {
		return false
	}
	return val == "true"
}

func (m *preemptionManager) addCandidate(candidate *Candidate) {
	podQueue, ok := m.pausedPods.Get(candidate.NodeName)
	if !ok {
		podQueue = priorityqueue.New(0)
	}
	priority := m.priorityFunc(candidate.Pod)
	item := priorityqueue.NewItem(toCacheKey(candidate.Pod), priority)
	podQueue.(priorityqueue.PriorityQueue).PushItem(item)
	m.pausedPods.Set(candidate.NodeName, podQueue, -1)
}

func (m *preemptionManager) removeCandidate(candidate *Candidate) {
	m.deadlineManager.RemovePodDeadline(candidate.Pod)
	pq, ok := m.pausedPods.Get(candidate.NodeName)
	if !ok {
		return
	}
	pq.(priorityqueue.PriorityQueue).RemoveItem(toCacheKey(candidate.Pod))
}

func (m *preemptionManager) PauseCandidate(ctx context.Context, candidate *Candidate) (*Candidate, error) {
	latestPod, err := m.podLister.Pods(candidate.Pod.Namespace).Get(candidate.Pod.Name)
	if err != nil {
		msg := "failed to list pod"
		klog.ErrorS(err, msg, "pod", klog.KObj(latestPod))
		return nil, fmt.Errorf("%s: %w", msg, err)
	}
	if len(latestPod.Spec.NodeName) <= 0 {
		return nil, ErrPodNotBoundToNode
	}
	if latestPod.Status.Phase == v1.PodPaused {
		return nil, ErrPodAlreadyPaused
	}
	markPodToPaused(latestPod)
	if err := updatePod(ctx, m.clientSet, latestPod); err != nil {
		msg := "failed to pause pod"
		klog.ErrorS(err, msg, "pod", klog.KObj(latestPod))
		return nil, fmt.Errorf("%s: %w", msg, err)
	}
	c := &Candidate{Pod: latestPod, NodeName: latestPod.Spec.NodeName}
	m.addCandidate(c)
	return c, nil
}

func (m *preemptionManager) GetPausedCandidateOnNode(ctx context.Context, nodeName string) *Candidate {
	if len(nodeName) <= 0 {
		return nil
	}
	res, ok := m.pausedPods.Get(nodeName)
	if !ok {
		return nil
	}
	q := res.(priorityqueue.PriorityQueue)
	item := q.PopItem()
	if item == nil {
		return nil
	}
	defer q.PushItem(item)
	namespace, name := parseCacheKey(item.Value())
	p, err := m.podLister.Pods(namespace).Get(name)
	if err != nil {
		return nil
	}
	return &Candidate{Pod: p, NodeName: p.Spec.NodeName}
}

func (m *preemptionManager) ResumeCandidate(ctx context.Context, candidate *Candidate) (*Candidate, error) {
	candidatePod, err := m.podLister.Pods(candidate.Pod.Namespace).Get(candidate.Pod.Name)
	if err != nil || candidatePod == nil {
		return nil, fmt.Errorf("failed to list pod: %w", err)
	}
	// pod cannot be resumed if it's has no noed assigned
	if len(candidate.NodeName) <= 0 || len(candidatePod.Spec.NodeName) <= 0 {
		return nil, ErrPodNotBoundToNode
	}
	res, ok := m.pausedPods.Get(candidate.NodeName)
	if !ok {
		return nil, ErrPodNotFound
	}
	podQueue := res.(priorityqueue.PriorityQueue)
	item := podQueue.GetItem(toCacheKey(candidatePod))
	if item == nil {
		return nil, ErrPodNotFound
	}
	// get node information of pod and dry run resume
	node, err := m.nodeLister.Get(candidatePod.Spec.NodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to list node: %w", err)
	}
	if err := m.dryRunResumeCandidate(candidatePod, node); err != nil {
		return nil, fmt.Errorf("failed to dry run resume pod: %w", err)
	}
	// resume pod
	markPodToResume(candidatePod)
	if err := updatePod(ctx, m.clientSet, candidatePod); err != nil {
		return nil, fmt.Errorf("failed to resume pod: %w", err)
	}
	c := &Candidate{NodeName: candidatePod.Spec.NodeName, Pod: candidatePod}
	m.removeCandidate(c)
	return c, nil
}

func (m preemptionManager) dryRunResumeCandidate(pod *v1.Pod, node *v1.Node) error {
	nodeInfo, err := m.nodeInfoLister.Get(node.Name)
	if err != nil {
		klog.ErrorS(err, "failed to get node info", "node", klog.KObj(node))
		return err
	}
	var unpausedPods []*v1.Pod
	for _, podInfo := range nodeInfo.Pods {
		if podInfo.Pod.Status.Phase != v1.PodPaused {
			unpausedPods = append(unpausedPods, podInfo.Pod)
		}
	}
	nodeInfoExcludePaused := framework.NewNodeInfo(unpausedPods...)
	nodeInfoExcludePaused.SetNode(nodeInfo.Node())
	if insufficientResources := noderesources.Fits(pod, nodeInfoExcludePaused); len(insufficientResources) > 0 {
		err := errors.New("insufficient resources to resume pod on node")
		klog.ErrorS(err, "cannot resume pod", "pod", klog.KObj(pod), "node", klog.KObj(node))
		return err
	}
	return nil
}

func toCacheKey(p *v1.Pod) string {
	return fmt.Sprintf("%s/%s", p.Namespace, p.Name)
}

func parseCacheKey(key string) (namespace string, name string) {
	res := strings.Split(key, "/")
	if len(res) != 2 {
		return
	}
	namespace = res[0]
	name = res[1]
	return
}

func markPodToResume(pod *v1.Pod) {
	annot := pod.Annotations
	if annot == nil {
		klog.V(5).InfoS("pod annotations is nil, creating a new map", "pod", klog.KObj(pod))
		annot = make(map[string]string)
	}
	annot[annotations.AnnotationKeyPausePod] = "false"
	pod.SetAnnotations(annot)
}

func markPodToPaused(pod *v1.Pod) {
	annot := pod.Annotations
	if annot == nil {
		klog.V(5).InfoS("pod annotations is nil, creating a new map", "pod", klog.KObj(pod))
		annot = make(map[string]string)
	}
	annot[annotations.AnnotationKeyPausePod] = "true"
	pod.SetAnnotations(annot)
}

// updatePod deletes the given <pod> from API server
func updatePod(ctx context.Context, cs kubernetes.Interface, pod *v1.Pod) error {
	_, err := cs.CoreV1().Pods(pod.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	return err
}
