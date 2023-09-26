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
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
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

type Candidate struct {
	NodeName string
	Pod      *v1.Pod
}

type Manager interface {
	// IsPodMarkedPaused checks if a pod is marked to be paused
	IsPodMarkedPaused(pod *v1.Pod) bool
	// AddPausedPod registers a candidate pod, candidate is assumed to be paused
	AddPausedPod(candidate *Candidate)
	// RemovePausedPod deregisters a paused pod
	RemovePausedPod(pod *v1.Pod)
	// GetPausedPodNode returns the node of a paused pod
	GetPausedPodNode(pod *v1.Pod) (*v1.Node, error)
	// PausePod checks if a currently paused pod should be resumed, then perform:
	//	1. update it to be resumed
	//	2. remove it from paused pod list
	//	3. return candidate pod
	// otherwise return nil
	ResumePausedPod(ctx context.Context, pod *v1.Pod) *Candidate
	// PauseCandidate set a candidate pod to paused
	PauseCandidate(ctx context.Context, pod *Candidate) error
}

// PreemptionManager maintains information related to current paused pods
type preemptionManager struct {
	pausedPods      *gocache.Cache
	deadlineManager deadline.Manager
	podLister       corelisters.PodLister
	nodeLister      corelisters.NodeLister
	nodeInfoLister  framework.NodeInfoLister
	clientSet       kubernetes.Interface
}

func NewPreemptionManager(podLister corelisters.PodLister, nodeLister corelisters.NodeLister, nodeInfoLister framework.NodeInfoLister, clientSet kubernetes.Interface) Manager {
	return &preemptionManager{
		pausedPods:      gocache.New(time.Second*5, time.Second*5),
		deadlineManager: deadline.NewDeadlineManager(),
		podLister:       podLister,
		nodeLister:      nodeLister,
		nodeInfoLister:  nodeInfoLister,
		clientSet:       clientSet,
	}
}

func (m *preemptionManager) IsPodMarkedPaused(pod *v1.Pod) bool {
	val, ok := pod.Annotations[AnnotationKeyPausePod]
	if !ok {
		return false
	}
	return val == "true"
}

func (m *preemptionManager) AddPausedPod(candidate *Candidate) {
	m.pausedPods.Set(toCacheKey(candidate.Pod), candidate.NodeName, -1)
	m.deadlineManager.AddPodDeadline(candidate.Pod)
}

func (m *preemptionManager) RemovePausedPod(pod *v1.Pod) {
	m.pausedPods.Delete(toCacheKey(pod))
	m.pausedPods.DeleteExpired()
	m.deadlineManager.RemovePodDeadline(pod)
}

func (m *preemptionManager) GetPausedPodNode(pod *v1.Pod) (*v1.Node, error) {
	nodeName, ok := m.pausedPods.Get(toCacheKey(pod))
	if !ok {
		return nil, ErrPodNotFound
	}
	node, err := m.nodeLister.Get(nodeName.(string))
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return node, nil
}

func (m *preemptionManager) ResumePausedPod(ctx context.Context, pod *v1.Pod) *Candidate {
	var candidates []*Candidate
	for key := range m.pausedPods.Items() {
		namespace, name := parseCacheKey(key)
		pausedPod, err := m.podLister.Pods(namespace).Get(name)
		if err != nil {
			klog.ErrorS(err, "failed to list pod", "pod", klog.KObj(pausedPod))
			continue
		}
		// // FIXME: pod might still be in pending state, which means it hasn't been paused yet
		// if pausedPod.Status.Phase != v1.PodPaused {
		// 	klog.InfoS("pod is no longer paused", "pod", klog.KObj(pausedPod))
		// 	m.RemovePausedPod(pod)
		// 	continue
		// }
		pausedPodDDL := m.deadlineManager.GetPodDeadline(pausedPod)
		podDDL := m.deadlineManager.GetPodDeadline(pod)
		if podDDL.Before(pausedPodDDL) {
			continue
		}
		node, err := m.GetPausedPodNode(pausedPod)
		if err != nil {
			klog.ErrorS(err, "failed to get node of paused pod", "pod", klog.KObj(pausedPod))
			continue
		}
		if err := m.dryRunResumePod(pausedPod, node); err != nil {
			klog.ErrorS(err, "failed to dry run resume pod", "pod", klog.KObj(pausedPod))
			continue
		}
		candidates = append(candidates, &Candidate{NodeName: node.Name, Pod: pausedPod})
	}

	if len(candidates) == 0 {
		return nil
	}
	candidate := candidates[0]
	minDDL := m.deadlineManager.GetPodDeadline(candidates[0].Pod)
	for _, c := range candidates {
		ddl := m.deadlineManager.GetPodDeadline(c.Pod)
		if ddl.Before(minDDL) {
			minDDL = ddl
			candidate = c
		}
	}
	markPodToResume(candidate.Pod)
	if err := updatePod(ctx, m.clientSet, candidate.Pod); err != nil {
		klog.ErrorS(err, "failed to resume pod", "pod", klog.KObj(candidate.Pod))
		return nil
	}
	m.RemovePausedPod(candidate.Pod)
	return candidate
}

func (m *preemptionManager) PauseCandidate(ctx context.Context, candidate *Candidate) error {
	latestPod, err := m.podLister.Pods(candidate.Pod.Namespace).Get(candidate.Pod.Name)
	if err != nil {
		msg := "failed to list pod"
		klog.ErrorS(err, msg, "pod", klog.KObj(latestPod))
		return fmt.Errorf("%s: %w", msg, err)
	}
	// TODO: if pod is already paused, no need to send pause again
	markPodToPaused(latestPod)
	if err := updatePod(ctx, m.clientSet, latestPod); err != nil {
		msg := "failed to pause pod"
		klog.ErrorS(err, msg, "pod", klog.KObj(latestPod))
		return fmt.Errorf("%s: %w", msg, err)
	}
	m.AddPausedPod(candidate)
	return nil
}

func (m preemptionManager) dryRunResumePod(pod *v1.Pod, node *v1.Node) error {
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
	annotations := pod.Annotations
	if annotations == nil {
		klog.InfoS("pod annotations is nil, creating a new map", "pod", klog.KObj(pod))
		annotations = make(map[string]string)
	}
	annotations[AnnotationKeyPausePod] = "false"
	pod.SetAnnotations(annotations)
}

func markPodToPaused(pod *v1.Pod) {
	annotations := pod.Annotations
	if annotations == nil {
		klog.InfoS("pod annotations is nil, creating a new map", "pod", klog.KObj(pod))
		annotations = make(map[string]string)
	}
	annotations[AnnotationKeyPausePod] = "true"
	pod.SetAnnotations(annotations)
}

// updatePod deletes the given <pod> from API server
func updatePod(ctx context.Context, cs kubernetes.Interface, pod *v1.Pod) error {
	_, err := cs.CoreV1().Pods(pod.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	return err
}
