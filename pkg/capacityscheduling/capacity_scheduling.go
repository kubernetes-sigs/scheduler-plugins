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

	"k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/core"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/util"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	externalv1alpha1 "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
	pluginsutil "sigs.k8s.io/scheduler-plugins/pkg/util"
)

// CapacityScheduling is a plugin that implements the mechanism of capacity scheduling.
type CapacityScheduling struct {
	sync.RWMutex
	frameworkHandle    framework.Handle
	podLister          corelisters.PodLister
	pdbLister          policylisters.PodDisruptionBudgetLister
	elasticQuotaLister externalv1alpha1.ElasticQuotaLister
	elasticQuotaInfos  ElasticQuotaInfos
}

// PreFilterState computed at PreFilter and used at PostFilter or Reserve.
type PreFilterState struct {
	framework.Resource
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
	args, ok := obj.(*config.CapacitySchedulingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type CapacitySchedulingArgs, got %T", obj)
	}
	kubeConfigPath := args.KubeConfigPath

	c := &CapacityScheduling{
		frameworkHandle:   handle,
		elasticQuotaInfos: NewElasticQuotaInfos(),
		podLister:         handle.SharedInformerFactory().Core().V1().Pods().Lister(),
		pdbLister:         getPDBLister(handle.SharedInformerFactory()),
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		return nil, err
	}
	client, err := versioned.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	schedSharedInformerFactory := schedinformer.NewSharedInformerFactory(client, 0)
	c.elasticQuotaLister = schedSharedInformerFactory.Scheduling().V1alpha1().ElasticQuotas().Lister()
	elasticQuotaInformer := schedSharedInformerFactory.Scheduling().V1alpha1().ElasticQuotas().Informer()
	elasticQuotaInformer.AddEventHandler(
		cache.FilteringResourceEventHandler{
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

	schedSharedInformerFactory.Start(nil)
	if !cache.WaitForCacheSync(nil, elasticQuotaInformer.HasSynced) {
		return nil, fmt.Errorf("timed out waiting for caches to sync %v", Name)
	}

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
	klog.Infof("CapacityScheduling start")
	return c, nil
}

// PreFilter performs the following validations.
// 1. Check if the (pod.request + eq.allocated) is less than eq.max.
// 2. Check if the sum(eq's usage) > sum(eq's min).
func (c *CapacityScheduling) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) *framework.Status {
	snapshotElasticQuota := c.snapshotElasticQuota()
	preFilterState := computePodResourceRequest(pod)

	state.Write(preFilterStateKey, preFilterState)
	state.Write(ElasticQuotaSnapshotKey, snapshotElasticQuota)

	elasticQuotaInfos := snapshotElasticQuota.elasticQuotaInfos
	eq := snapshotElasticQuota.elasticQuotaInfos[pod.Namespace]
	if eq == nil {
		return framework.NewStatus(framework.Success, "skipCapacityScheduling")
	}

	if eq.overUsed(preFilterState.Resource, eq.Max) {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Pod %v/%v is rejected in Prefilter because ElasticQuota %v is more than Max", pod.Namespace, pod.Name, eq.Namespace))
	}

	if elasticQuotaInfos.aggregatedMinOverUsedWithPod(preFilterState.Resource) {
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Pod %v/%v is rejected in Prefilter because total ElasticQuota used is more than min", pod.Namespace, pod.Name))
	}

	return framework.NewStatus(framework.Success, "")
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (c *CapacityScheduling) PreFilterExtensions() framework.PreFilterExtensions {
	return c
}

// AddPod from pre-computed data in cycleState.
func (c *CapacityScheduling) AddPod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podToAdd *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(cycleState)
	if err != nil {
		klog.Errorf("error reading %q from cycleState: %v", ElasticQuotaSnapshotKey, err)
		return framework.NewStatus(framework.Error, err.Error())
	}

	elasticQuotaInfo := elasticQuotaSnapshotState.elasticQuotaInfos[podToAdd.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.addPodIfNotPresent(podToAdd)
		if err != nil {
			klog.Errorf("ElasticQuota addPodIfNotPresent for pod %v/%v error %v", podToAdd.Namespace, podToAdd.Name, err)
		}
	}

	return framework.NewStatus(framework.Success, "")
}

// RemovePod from pre-computed data in cycleState.
func (c *CapacityScheduling) RemovePod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podToRemove *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(cycleState)
	if err != nil {
		klog.Errorf("error reading %q from cycleState: %v", ElasticQuotaSnapshotKey, err)
		return framework.NewStatus(framework.Error, err.Error())
	}

	elasticQuotaInfo := elasticQuotaSnapshotState.elasticQuotaInfos[podToRemove.Namespace]
	if elasticQuotaInfo != nil {
		err = elasticQuotaInfo.deletePodIfPresent(podToRemove)
		if err != nil {
			klog.Errorf("ElasticQuota deletePodIfPresent for pod %v/%v error %v", podToRemove.Namespace, podToRemove.Name, err)
		}
	}

	return framework.NewStatus(framework.Success, "")
}

func (c *CapacityScheduling) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	nnn, err := c.preempt(ctx, state, pod, filteredNodeStatusMap)
	if err != nil {
		return nil, framework.NewStatus(framework.Error, err.Error())
	}
	if nnn == "" {
		return nil, framework.NewStatus(framework.Unschedulable)
	}

	return &framework.PostFilterResult{NominatedNodeName: nnn}, framework.NewStatus(framework.Success)
}

func (c *CapacityScheduling) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	c.Lock()
	defer c.Unlock()

	elasticQuotaInfo := c.elasticQuotaInfos[pod.Namespace]
	if elasticQuotaInfo != nil {
		err := elasticQuotaInfo.addPodIfNotPresent(pod)
		if err != nil {
			klog.Errorf("ElasticQuota addPodIfNotPresent for pod %v/%v error %v", pod.Namespace, pod.Name, err)
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
			klog.Errorf("ElasticQuota deletePodIfPresent for pod %v/%v error %v", pod.Namespace, pod.Name, err)
		}
	}
}

func (c *CapacityScheduling) preempt(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (string, error) {
	client := c.frameworkHandle.ClientSet()
	ph := c.frameworkHandle.PreemptHandle()
	nodeLister := c.frameworkHandle.SnapshotSharedLister().NodeInfos()

	// Fetch the latest version of <pod>.
	// It's safe to directly fetch pod here. Because the informer cache has already been
	// initialized when creating the Scheduler obj, i.e., factory.go#MakeDefaultErrorFunc().
	// However, tests may need to manually initialize the shared pod informer.
	pod, err := c.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		klog.Errorf("Error getting the updated preemptor pod object: %v", err)
		return "", err
	}

	// 1) Ensure the preemptor is eligible to preempt other pods.
	if !defaultpreemption.PodEligibleToPreemptOthers(pod, nodeLister, m[pod.Status.NominatedNodeName]) {
		klog.V(5).Infof("Pod %v/%v is not eligible for more preemption.", pod.Namespace, pod.Name)
		return "", nil
	}

	// 2) Find all preemption candidates.
	candidates, err := FindCandidates(ctx, client, state, pod, m, ph, nodeLister, c.pdbLister)
	if err != nil || len(candidates) == 0 {
		return "", err
	}

	// 3) Interact with registered Extenders to filter out some candidates if needed.
	candidates, err = defaultpreemption.CallExtenders(ph.Extenders(), pod, nodeLister, candidates)
	if err != nil {
		return "", err
	}

	// 4) Find the best candidate.
	bestCandidate := defaultpreemption.SelectCandidate(candidates)
	if bestCandidate == nil || len(bestCandidate.Name()) == 0 {
		return "", nil
	}

	// 5) Perform preparation work before nominating the selected candidate.
	if err := defaultpreemption.PrepareCandidate(bestCandidate, c.frameworkHandle, client, pod); err != nil {
		return "", err
	}

	return bestCandidate.Name(), nil
}

// FindCandidates calculates a slice of preemption candidates.
// Each candidate is executable to make the given <pod> schedulable.
func FindCandidates(ctx context.Context, cs kubernetes.Interface, state *framework.CycleState, pod *v1.Pod,
	m framework.NodeToStatusMap, ph framework.PreemptHandle, nodeLister framework.NodeInfoLister,
	pdbLister policylisters.PodDisruptionBudgetLister) ([]defaultpreemption.Candidate, error) {
	allNodes, err := nodeLister.List()
	if err != nil {
		return nil, err
	}
	if len(allNodes) == 0 {
		return nil, core.ErrNoNodesAvailable
	}

	potentialNodes := nodesWherePreemptionMightHelp(allNodes, m)
	if len(potentialNodes) == 0 {
		klog.V(3).Infof("Preemption will not help schedule pod %v/%v on any node.", pod.Namespace, pod.Name)
		// In this case, we should clean-up any existing nominated node name of the pod.
		if err := util.ClearNominatedNodeName(cs, pod); err != nil {
			klog.Errorf("Cannot clear 'NominatedNodeName' field of pod %v/%v: %v", pod.Namespace, pod.Name, err)
			// We do not return as this error is not critical.
		}
		return nil, nil
	}
	if klog.V(5).Enabled() {
		var sample []string
		for i := 0; i < 10 && i < len(potentialNodes); i++ {
			sample = append(sample, potentialNodes[i].Node().Name)
		}
		klog.Infof("%v potential nodes for preemption, first %v are: %v", len(potentialNodes), len(sample), sample)
	}

	pdbs, err := getPodDisruptionBudgets(pdbLister)
	if err != nil {
		return nil, err
	}
	return dryRunPreemption(ctx, ph, state, pod, potentialNodes, pdbs), nil
}

// dryRunPreemption simulates Preemption logic on <potentialNodes> in parallel,
// and returns all possible preemption candidates.
func dryRunPreemption(ctx context.Context, fh framework.PreemptHandle, state *framework.CycleState,
	pod *v1.Pod, potentialNodes []*framework.NodeInfo, pdbs []*policy.PodDisruptionBudget) []defaultpreemption.Candidate {
	var resultLock sync.Mutex
	var candidates []defaultpreemption.Candidate
	checkNode := func(i int) {
		nodeInfoCopy := potentialNodes[i].Clone()
		stateCopy := state.Clone()

		pods, numPDBViolations, fits := selectVictimsOnNode(ctx, fh, stateCopy, pod, nodeInfoCopy, pdbs)
		if fits {
			resultLock.Lock()
			victims := extenderv1.Victims{
				Pods:             pods,
				NumPDBViolations: int64(numPDBViolations),
			}
			c := candidate{
				victims: &victims,
				name:    nodeInfoCopy.Node().Name,
			}
			candidates = append(candidates, &c)
			resultLock.Unlock()
		}
	}
	pluginsutil.Until(ctx, len(potentialNodes), checkNode)
	return candidates
}

// nodesWherePreemptionMightHelp returns a list of nodes with failed predicates
// that may be satisfied by removing pods from the node.
func nodesWherePreemptionMightHelp(nodes []*framework.NodeInfo, m framework.NodeToStatusMap) []*framework.NodeInfo {
	var potentialNodes []*framework.NodeInfo
	for _, node := range nodes {
		name := node.Node().Name
		// We reply on the status by each plugin - 'Unschedulable' or 'UnschedulableAndUnresolvable'
		// to determine whether preemption may help or not on the node.
		if m[name].Code() == framework.UnschedulableAndUnresolvable {
			continue
		}
		potentialNodes = append(potentialNodes, node)
	}
	return potentialNodes
}

func selectVictimsOnNode(
	ctx context.Context,
	ph framework.PreemptHandle,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget,
) ([]*v1.Pod, int, bool) {
	elasticQuotaSnapshotState, err := getElasticQuotaSnapshotState(state)
	if err != nil {
		klog.Errorf("error reading %q from cycleState: %v", ElasticQuotaSnapshotKey, err)
		return nil, 0, false
	}

	preFilterState, err := getPreFilterState(state)
	if err != nil {
		klog.Errorf("error reading %q from cycleState: %v", preFilterStateKey, err)
		return nil, 0, false
	}

	removePod := func(rp *v1.Pod) error {
		if err := nodeInfo.RemovePod(rp); err != nil {
			return err
		}
		status := ph.RunPreFilterExtensionRemovePod(ctx, state, pod, rp, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}
	addPod := func(ap *v1.Pod) error {
		nodeInfo.AddPod(ap)
		status := ph.RunPreFilterExtensionAddPod(ctx, state, pod, ap, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}

	elasticQuotaInfos := elasticQuotaSnapshotState.elasticQuotaInfos
	podPriority := corev1helpers.PodPriority(pod)
	preemptorElasticQuotaInfo, preemptorWithElasticQuota := elasticQuotaInfos[pod.Namespace]

	var moreThanMinWithPreemptor bool
	// Check if there is elastic quota in the preemptor's namespace.
	if preemptorWithElasticQuota {
		moreThanMinWithPreemptor = preemptorElasticQuotaInfo.overUsed(preFilterState.Resource, preemptorElasticQuotaInfo.Min)
	}

	// sort the pods in node by the priority class
	sort.Slice(nodeInfo.Pods, func(i, j int) bool { return !util.MoreImportantPod(nodeInfo.Pods[i].Pod, nodeInfo.Pods[j].Pod) })

	var potentialVictims []*v1.Pod
	if preemptorWithElasticQuota {
		for _, p := range nodeInfo.Pods {
			pElasticQuotaInfo, pWithElasticQuota := elasticQuotaInfos[p.Pod.Namespace]
			if !pWithElasticQuota {
				continue
			}

			if moreThanMinWithPreemptor {
				// If Preemptor.Request + Quota.Used > Quota.Min:
				// It means that its guaranteed isn't borrowed by other
				// quotas. So that we will select the pods which subject to the
				// same quota(namespace) with the lower priority than the
				// preemptor's priority as potential victims in a node.
				if p.Pod.Namespace == pod.Namespace && corev1helpers.PodPriority(p.Pod) < podPriority {
					potentialVictims = append(potentialVictims, p.Pod)
					if err := removePod(p.Pod); err != nil {
						return nil, 0, false
					}
				}

			} else {
				// If Preemptor.Request + Quota.allocated <= Quota.min: It
				// means that its min(guaranteed) resource is used or
				// `borrowed` by other Quota. Potential victims in a node
				// will be chosen from Quotas that allocates more resources
				// than its min, i.e., borrowing resources from other
				// Quotas.
				if p.Pod.Namespace != pod.Namespace && moreThanMin(*pElasticQuotaInfo.Used, *pElasticQuotaInfo.Min) {
					potentialVictims = append(potentialVictims, p.Pod)
					if err := removePod(p.Pod); err != nil {
						return nil, 0, false
					}
				}
			}
		}
	} else {
		for _, p := range nodeInfo.Pods {
			_, pWithElasticQuota := elasticQuotaInfos[p.Pod.Namespace]
			if pWithElasticQuota {
				continue
			}
			if corev1helpers.PodPriority(p.Pod) < podPriority {
				potentialVictims = append(potentialVictims, p.Pod)
				if err := removePod(p.Pod); err != nil {
					return nil, 0, false
				}
			}
		}
	}

	// No potential victims are found, and so we don't need to evaluate the node again since its state didn't change.
	if len(potentialVictims) == 0 {
		return nil, 0, false
	}

	// If the new pod does not fit after removing all the lower priority pods,
	// we are almost done and this node is not suitable for preemption. The only
	// condition that we could check is if the "pod" is failing to schedule due to
	// inter-pod affinity to one or more victims, but we have decided not to
	// support this case for performance reasons. Having affinity to lower
	// priority pods is not a recommended configuration anyway.
	if fits, _, err := core.PodPassesFiltersOnNode(ctx, ph, state, pod, nodeInfo); !fits {
		if err != nil {
			klog.Warningf("Encountered error while selecting victims on node %v: %v", nodeInfo.Node().Name, err)
		}

		return nil, 0, false
	}

	// If the quota.used + pod.request > quota.max or sum(quotas.used) + pod.request > sum(quotas.min)
	// after removing all the lower priority pods,
	// we are almost done and this node is not suitable for preemption.
	if preemptorWithElasticQuota {
		if preemptorElasticQuotaInfo.overUsed(preFilterState.Resource, preemptorElasticQuotaInfo.Max) ||
			elasticQuotaInfos.aggregatedMinOverUsedWithPod(preFilterState.Resource) {
			return nil, 0, false
		}
	}

	var victims []*v1.Pod
	numViolatingVictim := 0
	sort.Slice(potentialVictims, func(i, j int) bool { return util.MoreImportantPod(potentialVictims[i], potentialVictims[j]) })
	// Try to reprieve as many pods as possible. We first try to reprieve the PDB
	// violating victims and then other non-violating ones. In both cases, we start
	// from the highest priority victims.
	violatingVictims, nonViolatingVictims := filterPodsWithPDBViolation(potentialVictims, pdbs)
	reprievePod := func(p *v1.Pod) (bool, error) {
		if err := addPod(p); err != nil {
			return false, err
		}
		fits, _, _ := core.PodPassesFiltersOnNode(ctx, ph, state, pod, nodeInfo)
		if !fits {
			if err := removePod(p); err != nil {
				return false, err
			}
			victims = append(victims, p)
			klog.V(5).Infof("Pod %v/%v is a potential preemption victim on node %v.", p.Namespace, p.Name, nodeInfo.Node().Name)
		}

		if preemptorWithElasticQuota && (preemptorElasticQuotaInfo.overUsed(preFilterState.Resource, preemptorElasticQuotaInfo.Max) || elasticQuotaInfos.aggregatedMinOverUsedWithPod(preFilterState.Resource)) {
			if err := removePod(p); err != nil {
				return false, err
			}
			victims = append(victims, p)
			klog.V(5).Infof("Pod %v/%v is a potential preemption victim on node %v.", p.Namespace, p.Name, nodeInfo.Node().Name)
		}

		return fits, nil
	}
	for _, p := range violatingVictims {
		if fits, err := reprievePod(p); err != nil {
			klog.Warningf("Failed to reprieve pod %q: %v", p.Name, err)
			return nil, 0, false
		} else if !fits {
			numViolatingVictim++
		}
	}
	// Now we try to reprieve non-violating victims.
	for _, p := range nonViolatingVictims {
		if _, err := reprievePod(p); err != nil {
			klog.Warningf("Failed to reprieve pod %q: %v", p.Name, err)
			return nil, 0, false
		}
	}
	return victims, numViolatingVictim, true
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
		eqs, err := c.elasticQuotaLister.ElasticQuotas(pod.Namespace).List(labels.NewSelector())
		if err != nil {
			klog.Errorf("Get ElasticQuota %v error %v", pod.Namespace, err)
			return
		}

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
		klog.Errorf("ElasticQuota addPodIfNotPresent for pod %v/%v error %v", pod.Namespace, pod.Name, err)
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
				klog.Errorf("ElasticQuota deletePodIfPresent for pod %v/%v error %v", newPod.Namespace, newPod.Name, err)
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
			klog.Errorf("ElasticQuota deletePodIfPresent for pod %v/%v error %v", pod.Namespace, pod.Name, err)
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
		return nil, fmt.Errorf("error reading %q from cycleState: %v", preFilterStateKey, err)
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
		return nil, fmt.Errorf("error reading %q from cycleState: %v", ElasticQuotaSnapshotKey, err)
	}

	s, ok := c.(*ElasticQuotaSnapshotState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to CapacityScheduling ElasticQuotaSnapshotState error", c)
	}
	return s, nil
}

func getPDBLister(informerFactory informers.SharedInformerFactory) policylisters.PodDisruptionBudgetLister {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodDisruptionBudget) {
		return informerFactory.Policy().V1beta1().PodDisruptionBudgets().Lister()
	}
	return nil
}

func getPodDisruptionBudgets(pdbLister policylisters.PodDisruptionBudgetLister) ([]*policy.PodDisruptionBudget, error) {
	if pdbLister != nil {
		return pdbLister.List(labels.Everything())
	}
	return nil, nil
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
//   InitContainers
//     IC1:
//       CPU: 2
//       Memory: 1G
//     IC2:
//       CPU: 2
//       Memory: 3G
//   Containers
//     C1:
//       CPU: 2
//       Memory: 1G
//     C2:
//       CPU: 1
//       Memory: 1G
//
// Result: CPU: 3, Memory: 3G
func computePodResourceRequest(pod *v1.Pod) *PreFilterState {
	result := &PreFilterState{}
	for _, container := range pod.Spec.Containers {
		result.Add(container.Resources.Requests)
	}

	// take max_resource(sum_pod, any_init_container)
	for _, container := range pod.Spec.InitContainers {
		result.SetMaxResource(container.Resources.Requests)
	}

	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil && utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodOverhead) {
		result.Add(pod.Spec.Overhead)
	}

	return result
}

// filterPodsWithPDBViolation groups the given "pods" into two groups of "violatingPods"
// and "nonViolatingPods" based on whether their PDBs will be violated if they are
// preempted.
// This function is stable and does not change the order of received pods. So, if it
// receives a sorted list, grouping will preserve the order of the input list.
func filterPodsWithPDBViolation(pods []*v1.Pod, pdbs []*policy.PodDisruptionBudget) (violatingPods, nonViolatingPods []*v1.Pod) {
	pdbsAllowed := make([]int32, len(pdbs))
	for i, pdb := range pdbs {
		pdbsAllowed[i] = pdb.Status.DisruptionsAllowed
	}

	for _, obj := range pods {
		pod := obj
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
			violatingPods = append(violatingPods, pod)
		} else {
			nonViolatingPods = append(nonViolatingPods, pod)
		}
	}
	return violatingPods, nonViolatingPods
}

// assignedPod selects pods that are assigned (scheduled and running).
func assignedPod(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}
