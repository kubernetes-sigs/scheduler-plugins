/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package capacityscheduling

import (
	"context"
	"fmt"
	"sort"
	"sync"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
	ctrlruntimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// CapacityScheduling is a plugin that implements the mechanism of capacity scheduling.
type CapacityScheduling struct {
	sync.RWMutex
	fh                framework.Handle
	podLister         corelisters.PodLister
	pdbLister         policylisters.PodDisruptionBudgetLister
	client            client.Client
	elasticQuotaInfos ElasticQuotaInfos
}

// PreFilterState computed at PreFilter and used at PostFilter or Reserve.
type PreFilterState struct {
	podReq framework.Resource

	// nominatedPodsReqInEQWithPodReq is the sum of podReq and the requested resources of the Nominated Pods
	// which subject to the same quota(namespace) and is more important than the preemptor.
	nominatedPodsReqInEQWithPodReq framework.Resource

	// nominatedPodsReqWithPodReq is the sum of podReq and the requested resources of the Nominated Pods
	// which subject to the all quota(namespace). Generated Nominated Pods consist of two kinds of pods:
	// 1. the pods subject to the same quota(namespace) and is more important than the preemptor.
	// 2. the pods subject to the different quota(namespace) and the usage of quota(namespace) does not exceed min.
	nominatedPodsReqWithPodReq framework.Resource
}

// Clone the preFilter state.
func (s *PreFilterState) Clone() framework.StateData {
	return s
}

// ElasticQuotaSnapshotState stores the snapshot of elasticQuotas.
type ElasticQuotaSnapshotState struct {
	elasticQuotaInfos ElasticQuotaInfos
}

// Clone the ElasticQuotaSnapshot state.
func (s *ElasticQuotaSnapshotState) Clone() framework.StateData {
	return &ElasticQuotaSnapshotState{
		elasticQuotaInfos: s.elasticQuotaInfos.clone(),
	}
}

var _ framework.PreFilterPlugin = &CapacityScheduling{}
var _ framework.PostFilterPlugin = &CapacityScheduling{}
var _ framework.ReservePlugin = &CapacityScheduling{}
var _ framework.EnqueueExtensions = &CapacityScheduling{}
var _ preemption.Interface = &preemptor{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = "CapacityScheduling"

	// preFilterStateKey is the key in CycleState to NodeResourcesFit pre-computed data.
	preFilterStateKey       = "PreFilter" + Name
	ElasticQuotaSnapshotKey = "ElasticQuotaSnapshot"
)

// Name returns name of the plugin. It is used in logs, etc.
func (c *CapacityScheduling) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	c := &CapacityScheduling{
		fh:                handle,
		elasticQuotaInfos: NewElasticQuotaInfos(),
		podLister:         handle.SharedInformerFactory().Core().V1().Pods().Lister(),
		pdbLister:         getPDBLister(handle.SharedInformerFactory()),
	}

	client, err := client.New(handle.KubeConfig(), client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	c.client = client
	dynamicCache, err := ctrlruntimecache.New(handle.KubeConfig(), ctrlruntimecache.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	// TODO: pass in context.
	elasticQuotaInformer, err := dynamicCache.GetInformer(context.Background(), &v1alpha1.ElasticQuota{})
	if err != nil {
		return nil, err
	}
	elasticQuotaInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *v1alpha1.ElasticQuota:
				return true
			case cache.DeletedFinalStateUnknown:
				if _, ok := t.Obj.(*v1alpha1.ElasticQuota); ok {
					return true
				}
				utilruntime.HandleError(fmt.Errorf("cannot convert to *v1alpha1.ElasticQuota: %v", obj))
				return false
			default:
				utilruntime.HandleError(fmt.Errorf("unable to handle object in %T", obj))
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.addElasticQuota,
			UpdateFunc: c.updateElasticQuota,
			DeleteFunc: c.deleteElasticQuota,
		},
	})

	podInformer := handle.SharedInformerFactory().Core().V1().Pods().Informer()
	podInformer.AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return assignedPod(t)
				case cache.DeletedFinalStateUnknown:
					if pod, ok := t.Obj.(*v1.Pod); ok {
						return assignedPod(pod)
					}
					return false
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    c.addPod,
				UpdateFunc: c.updatePod,
				DeleteFunc: c.deletePod,
			},
		},
	)
	klog.InfoS("CapacityScheduling start")
	return c, nil
}

func (c *CapacityScheduling) EventsToRegister() []framework.ClusterEvent {
	// To register a custom event, follow the naming convention at:
	// https://git.k8s.io/kubernetes/pkg/scheduler/eventhandlers.go#L403-L410
	eqGVK := fmt.Sprintf("elasticquotas.v1alpha1.%v", scheduling.GroupName)
	return []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.Delete},
		{Resource: framework.GVK(eqGVK), ActionType: framework.All},
	}
}

// PreFilter performs the following validations.
// 1. Check if the (pod.request + eq.allocated) is less than eq.max.
// 2. Check if the sum(eq's usage) > sum(eq's min).
func (c *CapacityScheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// TODO improve the efficiency of taking snapshot
	// e.g. use a two-pointer data structure to only copy the updated EQs when necessary.
	snapshotElasticQuota := c.snapshotElasticQuota()
	podReq := computePodResourceRequest(pod)

	state.Write(ElasticQuotaSnapshotKey, snapshotElasticQuota)

	elasticQuotaInfos := snapshotElasticQuota.elasticQuotaInfos
	eq := snapshotElasticQuota.elasticQuotaInfos[pod.Namespace]
	if eq == nil {
		preFilterState := &PreFilterState{
			podReq: *podReq,
		}
		state.Write(preFilterStateKey, preFilterState)
		return nil, framework.NewStatus(framework.Success)
	}

	// nominatedPodsReqInEQWithPodReq is the sum of podReq and the requested resources of the Nominated Pods
	// which subject to the same quota(namespace) and is more important than the preemptor.
	nominatedPodsReqInEQWithPodReq := &framework.Resource{}
	// nominatedPodsReqWithPodReq is the sum of podReq and the requested resources of the Nominated Pods
	// which subject to the all quota(namespace). Generated Nominated Pods consist of two kinds of pods:
	// 1. the pods subject to the same quota(namespace) and is more important than the preemptor.
	// 2. the pods subject to the different quota(namespace) and the usage of quota(namespace) does not exceed min.
	nominatedPodsReqWithPodReq := &framework.Resource{}

	nodeList, err := c.fh.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, fmt.Sprintf("Error getting the nodelist: %v", err))
	}

	for _, node := range nodeList {
		nominatedPods := c.fh.NominatedPodsForNode(node.Node().Name)
		for _, p := range nominatedPods {
			if p.Pod.UID == pod.UID {
				continue
			}
			ns := p.Pod.Namespace
			info := c.elasticQuotaInfos[ns]
			if info != nil {
				pResourceRequest := util.ResourceList(computePodResourceRequest(p.Pod))
				// If they are subject to the same quota(namespace) and p is more important than pod,
				// p will be added to the nominatedResource and totalNominatedResource.
				// If they aren't subject to the same quota(namespace) and the usage of quota(p's namespace) does not exceed min,
				// p will be added to the totalNominatedResource.
				if ns == pod.Namespace && corev1helpers.PodPriority(p.Pod) >= corev1helpers.PodPriority(pod) {
					nominatedPodsReqInEQWithPodReq.Add(pResourceRequest)
					nominatedPodsReqWithPodReq.Add(pResourceRequest)
				} else if ns != pod.Namespace && !info.usedOverMin() {
					nominatedPodsReqWithPodReq.Add(pResourceRequest)
				}
			}
		}
	}

	nominatedPodsReqInEQWithPodReq.Add(util.ResourceList(podReq))
	nominatedPodsReqWithPodReq.Add(util.ResourceList(podReq))
	preFilterState := &PreFilterState{
		podReq:                         *podReq,
		nominatedPodsReqInEQWithPodReq: *nominatedPodsReqInEQWithPodReq,
		nominatedPodsReqWithPodReq:     *nominatedPodsReqWithPodReq,
	}
	state.Write(preFilterStateKey, preFilterState)

	if eq.usedOverMaxWith(nominatedPodsReqInEQWithPodReq) {
		return nil, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Pod %v/%v is rejected in PreFilter because ElasticQuota %v is more than Max", pod.Namespace, pod.Name, eq.Namespace))
	}

	if elasticQuotaInfos.aggregatedUsedOverMinWith(*nominatedPodsReqWithPodReq) {
		return nil, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Pod %v/%v is rejected in PreFilter because total ElasticQuota used is more than min", pod.Namespace, pod.Name))
	}

	return nil, framework.NewStatus(framework.Success, "")
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (c *CapacityScheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return c
}

// AddPod from pre-computed data in cycleState.
func (c *CapacityScheduling) AddPod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(cycleState)
	if err != nil {
		klog.ErrorS(err, "Failed to read elasticQuotaSnapshot from cycleState", "elasticQuotaSnapshotKey", ElasticQuotaSnapshotKey)
		return framework.NewStatus(framework.Error, err.Error())
	}

	elasticQuotaInfo := elasticQuotaSnapshotState.elasticQuotaInfos[podToAdd.Pod.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.addPodIfNotPresent(podToAdd.Pod)
		if err != nil {
			klog.ErrorS(err, "Failed to add Pod to its associated elasticQuota", "pod", klog.KObj(podToAdd.Pod))
		}
	}

	return framework.NewStatus(framework.Success, "")
}

// RemovePod from pre-computed data in cycleState.
func (c *CapacityScheduling) RemovePod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(cycleState)
	if err != nil {
		klog.ErrorS(err, "Failed to read elasticQuotaSnapshot from cycleState", "elasticQuotaSnapshotKey", ElasticQuotaSnapshotKey)
		return framework.NewStatus(framework.Error, err.Error())
	}

	elasticQuotaInfo := elasticQuotaSnapshotState.elasticQuotaInfos[podToRemove.Pod.Namespace]
	if elasticQuotaInfo != nil {
		err = elasticQuotaInfo.deletePodIfPresent(podToRemove.Pod)
		if err != nil {
			klog.ErrorS(err, "Failed to delete Pod from its associated elasticQuota", "pod", klog.KObj(podToRemove.Pod))
		}
	}

	return framework.NewStatus(framework.Success, "")
}

func (c *CapacityScheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	defer func() {
		metrics.PreemptionAttempts.Inc()
	}()

	pe := preemption.Evaluator{
		PluginName: c.Name(),
		Handler:    c.fh,
		PodLister:  c.podLister,
		PdbLister:  c.pdbLister,
		State:      state,
		Interface: &preemptor{
			fh:    c.fh,
			state: state,
		},
	}

	return pe.Preempt(ctx, pod, m)
}

func (c *CapacityScheduling) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	c.Lock()
	defer c.Unlock()

	elasticQuotaInfo := c.elasticQuotaInfos[pod.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.addPodIfNotPresent(pod)
		if err != nil {
			klog.ErrorS(err, "Failed to add Pod to its associated elasticQuota", "pod", klog.KObj(pod))
			return framework.NewStatus(framework.Error, err.Error())
		}
	}
	return framework.NewStatus(framework.Success, "")
}

func (c *CapacityScheduling) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	c.Lock()
	defer c.Unlock()

	elasticQuotaInfo := c.elasticQuotaInfos[pod.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.deletePodIfPresent(pod)
		if err != nil {
			klog.ErrorS(err, "Failed to delete Pod from its associated elasticQuota", "pod", klog.KObj(pod))
		}
	}
}

type preemptor struct {
	fh    framework.Handle
	state *framework.CycleState
}

func (p *preemptor) GetOffsetAndNumCandidates(n int32) (int32, int32) {
	return 0, n
}

func (p *preemptor) CandidatesToVictimsMap(candidates []preemption.Candidate) map[string]*extenderv1.Victims {
	m := make(map[string]*extenderv1.Victims)
	for _, c := range candidates {
		m[c.Name()] = c.Victims()
	}
	return m
}

// PodEligibleToPreemptOthers determines whether this pod should be considered
// for preempting other pods or not. If this pod has already preempted other
// pods and those are in their graceful termination period, it shouldn't be
// considered for preemption.
// We look at the node that is nominated for this pod and as long as there are
// terminating pods on the node, we don't consider this for preempting more pods.
func (p *preemptor) PodEligibleToPreemptOthers(pod *v1.Pod, nominatedNodeStatus *framework.Status) (bool, string) {
	if pod.Spec.PreemptionPolicy != nil && *pod.Spec.PreemptionPolicy == v1.PreemptNever {
		klog.V(5).InfoS("Pod is not eligible for preemption because of its preemptionPolicy", "pod", klog.KObj(pod), "preemptionPolicy", v1.PreemptNever)
		return false, "not eligible due to preemptionPolicy=Never."
	}

	preFilterState, err := getPreFilterState(p.state)
	if err != nil {
		klog.ErrorS(err, "Failed to read preFilterState from cycleState", "preFilterStateKey", preFilterStateKey)
		return false, "not eligible due to failed to read from cycleState"
	}

	nomNodeName := pod.Status.NominatedNodeName
	nodeLister := p.fh.SnapshotSharedLister().NodeInfos()
	if len(nomNodeName) > 0 {
		// If the pod's nominated node is considered as UnschedulableAndUnresolvable by the filters,
		// then the pod should be considered for preempting again.
		if nominatedNodeStatus.Code() == framework.UnschedulableAndUnresolvable {
			return true, ""
		}

		elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(p.state)
		if err != nil {
			klog.ErrorS(err, "Failed to read elasticQuotaSnapshot from cycleState", "elasticQuotaSnapshotKey", ElasticQuotaSnapshotKey)
			return true, ""
		}

		nodeInfo, _ := nodeLister.Get(nomNodeName)
		if nodeInfo == nil {
			return true, ""
		}

		podPriority := corev1helpers.PodPriority(pod)
		preemptorEQInfo, preemptorWithEQ := elasticQuotaSnapshotState.elasticQuotaInfos[pod.Namespace]
		if preemptorWithEQ {
			moreThanMinWithPreemptor := preemptorEQInfo.usedOverMinWith(&preFilterState.nominatedPodsReqInEQWithPodReq)
			for _, p := range nodeInfo.Pods {
				// Checking terminating pods
				if p.Pod.DeletionTimestamp != nil {
					eqInfo, withEQ := elasticQuotaSnapshotState.elasticQuotaInfos[p.Pod.Namespace]
					if !withEQ {
						continue
					}
					if p.Pod.Namespace == pod.Namespace && corev1helpers.PodPriority(p.Pod) < podPriority {
						// There is a terminating pod on the nominated node.
						// If the terminating pod is in the same namespace with preemptor
						// and it is less important than preemptor,
						// return false to avoid preempting more pods.
						return false, "not eligible due to a terminating pod on the nominated node."
					} else if p.Pod.Namespace != pod.Namespace && !moreThanMinWithPreemptor && eqInfo.usedOverMin() {
						// There is a terminating pod on the nominated node.
						// The terminating pod isn't in the same namespace with preemptor.
						// If moreThanMinWithPreemptor is false, it indicates that preemptor can preempt the pods in other EQs whose used is over min.
						// And if the used of terminating pod's quota is over min, so the room released by terminating pod on the nominated node can be used by the preemptor.
						// return false to avoid preempting more pods.
						return false, "not eligible due to a terminating pod on the nominated node."
					}
				}
			}
		} else {
			for _, p := range nodeInfo.Pods {
				_, withEQ := elasticQuotaSnapshotState.elasticQuotaInfos[p.Pod.Namespace]
				if withEQ {
					continue
				}
				if p.Pod.DeletionTimestamp != nil && corev1helpers.PodPriority(p.Pod) < podPriority {
					// There is a terminating pod on the nominated node.
					return false, "not eligible due to a terminating pod on the nominated node."
				}
			}
		}
	}
	return true, ""
}

func (p *preemptor) SelectVictimsOnNode(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget) ([]*v1.Pod, int, *framework.Status) {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(state)
	if err != nil {
		msg := "Failed to read elasticQuotaSnapshot from cycleState"
		klog.ErrorS(err, msg, "elasticQuotaSnapshotKey", ElasticQuotaSnapshotKey)
		return nil, 0, framework.NewStatus(framework.Unschedulable, msg)
	}

	preFilterState, err := getPreFilterState(state)
	if err != nil {
		msg := "Failed to read preFilterState from cycleState"
		klog.ErrorS(err, msg, "preFilterStateKey", preFilterStateKey)
		return nil, 0, framework.NewStatus(framework.Unschedulable, msg)
	}

	var nominatedPodsReqInEQWithPodReq framework.Resource
	var nominatedPodsReqWithPodReq framework.Resource
	podReq := preFilterState.podReq

	removePod := func(rpi *framework.PodInfo) error {
		if err := nodeInfo.RemovePod(rpi.Pod); err != nil {
			return err
		}
		status := p.fh.RunPreFilterExtensionRemovePod(ctx, state, pod, rpi, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}
	addPod := func(api *framework.PodInfo) error {
		nodeInfo.AddPodInfo(api)
		status := p.fh.RunPreFilterExtensionAddPod(ctx, state, pod, api, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}

	elasticQuotaInfos := elasticQuotaSnapshotState.elasticQuotaInfos
	podPriority := corev1helpers.PodPriority(pod)
	preemptorElasticQuotaInfo, preemptorWithElasticQuota := elasticQuotaInfos[pod.Namespace]

	// sort the pods in node by the priority class
	sort.Slice(nodeInfo.Pods, func(i, j int) bool { return !schedutil.MoreImportantPod(nodeInfo.Pods[i].Pod, nodeInfo.Pods[j].Pod) })

	var potentialVictims []*framework.PodInfo
	if preemptorWithElasticQuota {
		nominatedPodsReqInEQWithPodReq = preFilterState.nominatedPodsReqInEQWithPodReq
		nominatedPodsReqWithPodReq = preFilterState.nominatedPodsReqWithPodReq
		moreThanMinWithPreemptor := preemptorElasticQuotaInfo.usedOverMinWith(&nominatedPodsReqInEQWithPodReq)
		for _, p := range nodeInfo.Pods {
			eqInfo, withEQ := elasticQuotaInfos[p.Pod.Namespace]
			if !withEQ {
				continue
			}

			if moreThanMinWithPreemptor {
				// If Preemptor.Request + Quota.Used > Quota.Min:
				// It means that its guaranteed isn't borrowed by other
				// quotas. So that we will select the pods which subject to the
				// same quota(namespace) with the lower priority than the
				// preemptor's priority as potential victims in a node.
				if p.Pod.Namespace == pod.Namespace && corev1helpers.PodPriority(p.Pod) < podPriority {
					potentialVictims = append(potentialVictims, p)
					if err := removePod(p); err != nil {
						return nil, 0, framework.AsStatus(err)
					}
				}

			} else {
				// If Preemptor.Request + Quota.allocated <= Quota.min: It
				// means that its min(guaranteed) resource is used or
				// `borrowed` by other Quota. Potential victims in a node
				// will be chosen from Quotas that allocates more resources
				// than its min, i.e., borrowing resources from other
				// Quotas.
				if p.Pod.Namespace != pod.Namespace && eqInfo.usedOverMin() {
					potentialVictims = append(potentialVictims, p)
					if err := removePod(p); err != nil {
						return nil, 0, framework.AsStatus(err)
					}
				}
			}
		}
	} else {
		for _, p := range nodeInfo.Pods {
			_, withEQ := elasticQuotaInfos[p.Pod.Namespace]
			if withEQ {
				continue
			}
			if corev1helpers.PodPriority(p.Pod) < podPriority {
				potentialVictims = append(potentialVictims, p)
				if err := removePod(p); err != nil {
					return nil, 0, framework.AsStatus(err)
				}
			}
		}
	}

	// No potential victims are found, and so we don't need to evaluate the node again since its state didn't change.
	if len(potentialVictims) == 0 {
		message := fmt.Sprintf("No victims found on node %v for preemptor pod %v", nodeInfo.Node().Name, pod.Name)
		return nil, 0, framework.NewStatus(framework.UnschedulableAndUnresolvable, message)
	}

	// If the new pod does not fit after removing all the lower priority pods,
	// we are almost done and this node is not suitable for preemption. The only
	// condition that we could check is if the "pod" is failing to schedule due to
	// inter-pod affinity to one or more victims, but we have decided not to
	// support this case for performance reasons. Having affinity to lower
	// priority pods is not a recommended configuration anyway.
	if s := p.fh.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfo); !s.IsSuccess() {
		return nil, 0, s
	}

	// If the quota.used + pod.request > quota.max or sum(quotas.used) + pod.request > sum(quotas.min)
	// after removing all the lower priority pods,
	// we are almost done and this node is not suitable for preemption.
	if preemptorWithElasticQuota {
		if preemptorElasticQuotaInfo.usedOverMaxWith(&podReq) ||
			elasticQuotaInfos.aggregatedUsedOverMinWith(podReq) {
			return nil, 0, framework.NewStatus(framework.Unschedulable, "global quota max exceeded")
		}
	}

	var victims []*v1.Pod
	numViolatingVictim := 0
	sort.Slice(potentialVictims, func(i, j int) bool {
		return schedutil.MoreImportantPod(potentialVictims[i].Pod, potentialVictims[j].Pod)
	})
	// Try to reprieve as many pods as possible. We first try to reprieve the PDB
	// violating victims and then other non-violating ones. In both cases, we start
	// from the highest priority victims.
	violatingVictims, nonViolatingVictims := filterPodsWithPDBViolation(potentialVictims, pdbs)
	reprievePod := func(pi *framework.PodInfo) (bool, error) {
		if err := addPod(pi); err != nil {
			return false, err
		}
		s := p.fh.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfo)
		fits := s.IsSuccess()
		if !fits {
			if err := removePod(pi); err != nil {
				return false, err
			}
			victims = append(victims, pi.Pod)
			klog.V(5).InfoS("Found a potential preemption victim on node", "pod", klog.KObj(pi.Pod), "node", klog.KObj(nodeInfo.Node()))
		}

		if preemptorWithElasticQuota && (preemptorElasticQuotaInfo.usedOverMaxWith(&nominatedPodsReqInEQWithPodReq) || elasticQuotaInfos.aggregatedUsedOverMinWith(nominatedPodsReqWithPodReq)) {
			if err := removePod(pi); err != nil {
				return false, err
			}
			victims = append(victims, pi.Pod)
			klog.V(5).InfoS("Found a potential preemption victim on node", "pod", klog.KObj(pi.Pod), " node", klog.KObj(nodeInfo.Node()))
		}

		return fits, nil
	}
	for _, pi := range violatingVictims {
		if fits, err := reprievePod(pi); err != nil {
			klog.ErrorS(err, "Failed to reprieve pod", "pod", klog.KObj(pi.Pod))
			return nil, 0, framework.AsStatus(err)
		} else if !fits {
			numViolatingVictim++
		}
	}
	// Now we try to reprieve non-violating victims.
	for _, pi := range nonViolatingVictims {
		if _, err := reprievePod(pi); err != nil {
			klog.ErrorS(err, "Failed to reprieve pod", "pod", klog.KObj(pi.Pod))
			return nil, 0, framework.AsStatus(err)
		}
	}
	return victims, numViolatingVictim, framework.NewStatus(framework.Success)
}

func (c *CapacityScheduling) addElasticQuota(obj interface{}) {
	eq := obj.(*v1alpha1.ElasticQuota)
	oldElasticQuotaInfo := c.elasticQuotaInfos[eq.Namespace]
	if oldElasticQuotaInfo != nil {
		return
	}

	elasticQuotaInfo := newElasticQuotaInfo(eq.Namespace, eq.Spec.Min, eq.Spec.Max, nil)

	c.Lock()
	defer c.Unlock()
	c.elasticQuotaInfos[eq.Namespace] = elasticQuotaInfo
}

func (c *CapacityScheduling) updateElasticQuota(oldObj, newObj interface{}) {
	oldEQ := oldObj.(*v1alpha1.ElasticQuota)
	newEQ := newObj.(*v1alpha1.ElasticQuota)
	newEQInfo := newElasticQuotaInfo(newEQ.Namespace, newEQ.Spec.Min, newEQ.Spec.Max, nil)

	c.Lock()
	defer c.Unlock()

	oldEQInfo := c.elasticQuotaInfos[oldEQ.Namespace]
	if oldEQInfo != nil {
		newEQInfo.pods = oldEQInfo.pods
		newEQInfo.Used = oldEQInfo.Used
	}
	c.elasticQuotaInfos[newEQ.Namespace] = newEQInfo
}

func (c *CapacityScheduling) deleteElasticQuota(obj interface{}) {
	elasticQuota := obj.(*v1alpha1.ElasticQuota)
	c.Lock()
	defer c.Unlock()
	delete(c.elasticQuotaInfos, elasticQuota.Namespace)
}

func (c *CapacityScheduling) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)

	c.Lock()
	defer c.Unlock()

	elasticQuotaInfo := c.elasticQuotaInfos[pod.Namespace]
	// If elasticQuotaInfo is nil, try to list ElasticQuotas through elasticQuotaLister
	if elasticQuotaInfo == nil {
		var eqList v1alpha1.ElasticQuotaList
		if err := c.client.List(context.Background(), &eqList, client.InNamespace(pod.Namespace)); err != nil {
			klog.ErrorS(err, "Failed to get elasticQuota", "elasticQuota", pod.Namespace)
			return
		}

		eqs := eqList.Items
		// If the length of elasticQuotas is 0, return.
		if len(eqs) == 0 {
			return
		}

		if len(eqs) > 0 {
			// only one elasticquota is supported in each namespace
			eq := eqs[0]
			elasticQuotaInfo = newElasticQuotaInfo(eq.Namespace, eq.Spec.Min, eq.Spec.Max, nil)
			c.elasticQuotaInfos[eq.Namespace] = elasticQuotaInfo
		}
	}

	err := elasticQuotaInfo.addPodIfNotPresent(pod)
	if err != nil {
		klog.ErrorS(err, "Failed to add Pod to its associated elasticQuota", "pod", klog.KObj(pod))
	}
}

func (c *CapacityScheduling) updatePod(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	if oldPod.Status.Phase == v1.PodSucceeded || oldPod.Status.Phase == v1.PodFailed {
		return
	}

	if newPod.Status.Phase != v1.PodRunning && newPod.Status.Phase != v1.PodPending {
		c.Lock()
		defer c.Unlock()

		elasticQuotaInfo := c.elasticQuotaInfos[newPod.Namespace]
		if elasticQuotaInfo != nil {
			err := elasticQuotaInfo.deletePodIfPresent(newPod)
			if err != nil {
				klog.ErrorS(err, "Failed to delete Pod from its associated elasticQuota", "pod", klog.KObj(newPod))
			}
		}
	}
}

func (c *CapacityScheduling) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	c.Lock()
	defer c.Unlock()

	elasticQuotaInfo := c.elasticQuotaInfos[pod.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.deletePodIfPresent(pod)
		if err != nil {
			klog.ErrorS(err, "Failed to delete Pod from its associated elasticQuota", "pod", klog.KObj(pod))
		}
	}
}

// getElasticQuotasSnapshot will return the snapshot of elasticQuotas.
func (c *CapacityScheduling) snapshotElasticQuota() *ElasticQuotaSnapshotState {
	c.RLock()
	defer c.RUnlock()

	elasticQuotaInfosDeepCopy := c.elasticQuotaInfos.clone()
	return &ElasticQuotaSnapshotState{
		elasticQuotaInfos: elasticQuotaInfosDeepCopy,
	}
}

func getPreFilterState(cycleState *framework.CycleState) (*PreFilterState, error) {
	c, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("error reading %q from cycleState: %w", preFilterStateKey, err)
	}

	s, ok := c.(*PreFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to NodeResourcesFit.preFilterState error", c)
	}
	return s, nil
}

func getElasticQuotaSnapshotState(cycleState *framework.CycleState) (*ElasticQuotaSnapshotState, error) {
	c, err := cycleState.Read(ElasticQuotaSnapshotKey)
	if err != nil {
		// ElasticQuotaSnapshotState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("error reading %q from cycleState: %w", ElasticQuotaSnapshotKey, err)
	}

	s, ok := c.(*ElasticQuotaSnapshotState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to CapacityScheduling ElasticQuotaSnapshotState error", c)
	}
	return s, nil
}

func getPDBLister(informerFactory informers.SharedInformerFactory) policylisters.PodDisruptionBudgetLister {
	return informerFactory.Policy().V1().PodDisruptionBudgets().Lister()
}

// computePodResourceRequest returns a framework.Resource that covers the largest
// width in each resource dimension. Because init-containers run sequentially, we collect
// the max in each dimension iteratively. In contrast, we sum the resource vectors for
// regular containers since they run simultaneously.
//
// If Pod Overhead is specified and the feature gate is set, the resources defined for Overhead
// are added to the calculated Resource request sum
//
// Example:
//
// Pod:
//
//	InitContainers
//	  IC1:
//	    CPU: 2
//	    Memory: 1G
//	  IC2:
//	    CPU: 2
//	    Memory: 3G
//	Containers
//	  C1:
//	    CPU: 2
//	    Memory: 1G
//	  C2:
//	    CPU: 1
//	    Memory: 1G
//
// Result: CPU: 3, Memory: 3G
func computePodResourceRequest(pod *v1.Pod) *framework.Resource {
	result := &framework.Resource{}
	for _, container := range pod.Spec.Containers {
		result.Add(container.Resources.Requests)
	}

	// take max_resource(sum_pod, any_init_container)
	for _, container := range pod.Spec.InitContainers {
		result.SetMaxResource(container.Resources.Requests)
	}

	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil {
		result.Add(pod.Spec.Overhead)
	}

	return result
}

// filterPodsWithPDBViolation groups the given "pods" into two groups of "violatingPods"
// and "nonViolatingPods" based on whether their PDBs will be violated if they are
// preempted.
// This function is stable and does not change the order of received pods. So, if it
// receives a sorted list, grouping will preserve the order of the input list.
func filterPodsWithPDBViolation(podInfos []*framework.PodInfo, pdbs []*policy.PodDisruptionBudget) (violatingPods, nonViolatingPods []*framework.PodInfo) {
	pdbsAllowed := make([]int32, len(pdbs))
	for i, pdb := range pdbs {
		pdbsAllowed[i] = pdb.Status.DisruptionsAllowed
	}

	for _, podInfo := range podInfos {
		pod := podInfo.Pod
		pdbForPodIsViolated := false
		// A pod with no labels will not match any PDB. So, no need to check.
		if len(pod.Labels) != 0 {
			for i, pdb := range pdbs {
				if pdb.Namespace != pod.Namespace {
					continue
				}
				selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
				if err != nil {
					continue
				}
				// A PDB with a nil or empty selector matches nothing.
				if selector.Empty() || !selector.Matches(labels.Set(pod.Labels)) {
					continue
				}

				// Existing in DisruptedPods means it has been processed in API server,
				// we don't treat it as a violating case.
				if _, exist := pdb.Status.DisruptedPods[pod.Name]; exist {
					continue
				}
				// Only decrement the matched pdb when it's not in its <DisruptedPods>;
				// otherwise we may over-decrement the budget number.
				pdbsAllowed[i]--
				// We have found a matching PDB.
				if pdbsAllowed[i] < 0 {
					pdbForPodIsViolated = true
				}
			}
		}
		if pdbForPodIsViolated {
			violatingPods = append(violatingPods, podInfo)
		} else {
			nonViolatingPods = append(nonViolatingPods, podInfo)
		}
	}
	return violatingPods, nonViolatingPods
}

// assignedPod selects pods that are assigned (scheduled and running).
func assignedPod(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}
