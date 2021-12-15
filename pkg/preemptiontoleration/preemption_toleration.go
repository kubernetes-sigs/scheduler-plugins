/*
Copyright 2021 The Kubernetes Authors.

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

package preemptiontoleration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1beta1"
	schedulinglisters "k8s.io/client-go/listers/scheduling/v1"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	schedulerapisconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/validation"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	dp "k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	// Name of the plugin used in the plugin registry and configurations.
	Name = "PreemptionToleration"
)

var (
	_ framework.PostFilterPlugin = &PreemptionToleration{}
)

// PreemptionToleration is a PostFilter plugin implements the preemption logic.
type PreemptionToleration struct {
	fh                  framework.Handle
	args                config.PreemptionTolerationArgs
	podLister           corelisters.PodLister
	pdbLister           policylisters.PodDisruptionBudgetLister
	priorityClassLister schedulinglisters.PriorityClassLister
	clock               util.Clock
}

var _ framework.PostFilterPlugin = &PreemptionToleration{}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *PreemptionToleration) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(rawArgs runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	args, ok := rawArgs.(*config.PreemptionTolerationArgs)
	if !ok {
		return nil, fmt.Errorf("got args of type %T, want *PreemptionTolerationArgs", args)
	}

	if err := validation.ValidateDefaultPreemptionArgs(field.NewPath(""), (*schedulerapisconfig.DefaultPreemptionArgs)(args)); err != nil {
		return nil, err
	}

	pl := PreemptionToleration{
		fh:                  fh,
		args:                *args,
		podLister:           fh.SharedInformerFactory().Core().V1().Pods().Lister(),
		priorityClassLister: fh.SharedInformerFactory().Scheduling().V1().PriorityClasses().Lister(),
		pdbLister:           getPDBLister(fh.SharedInformerFactory()),
		clock:               util.RealClock{},
	}
	return &pl, nil
}

// PostFilter invoked at the postFilter extension point.
func (pl *PreemptionToleration) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	defer func() {
		metrics.PreemptionAttempts.Inc()
	}()

	nnn, status := pl.preempt(ctx, state, pod, m)
	if !status.IsSuccess() {
		return nil, status
	}
	// This happens when the pod is not eligible for preemption or extenders filtered all candidates.
	if nnn == "" {
		return nil, framework.NewStatus(framework.Unschedulable)
	}
	return &framework.PostFilterResult{NominatedNodeName: nnn}, framework.NewStatus(framework.Success)
}

func (pl *PreemptionToleration) preempt(ctx context.Context, state *framework.CycleState, preemptor *v1.Pod, m framework.NodeToStatusMap) (string, *framework.Status) {
	cs := pl.fh.ClientSet()
	nodeLister := pl.fh.SnapshotSharedLister().NodeInfos()

	// 0) Fetch the latest version of <preemptor>.
	// It's safe to directly fetch pod here. Because the informer cache has already been
	// initialized when creating the Scheduler obj, i.e., factory.go#MakeDefaultErrorFunc().
	// However, tests may need to manually initialize the shared pod informer.
	preemptorNamespace, preemptorName := preemptor.Namespace, preemptor.Name
	preemptor, err := pl.podLister.Pods(preemptor.Namespace).Get(preemptor.Name)
	if err != nil {
		klog.ErrorS(err, "getting the updated preemptor pod object", "pod", klog.KRef(preemptorNamespace, preemptorName))
		return "", framework.AsStatus(err)
	}

	// 1) Ensure the preemptor is eligible to preempt other pods.
	if !dp.PodEligibleToPreemptOthers(preemptor, nodeLister, m[preemptor.Status.NominatedNodeName]) {
		klog.V(5).InfoS("Pod is not eligible for more preemption", "pod", klog.KObj(preemptor))
		return "", nil
	}

	// 2) Find all preemption candidates.
	candidates, nodeToStatusMap, status := pl.FindCandidates(ctx, state, preemptor, m)
	if !status.IsSuccess() {
		return "", status
	}

	// Return a FitError only when there are no candidates that fit the preemptor.
	if len(candidates) == 0 {
		fitError := &framework.FitError{
			Pod:         preemptor,
			NumAllNodes: len(nodeToStatusMap),
			Diagnosis: framework.Diagnosis{
				NodeToStatusMap: nodeToStatusMap,
				// Leave FailedPlugins as nil as it won't be used on moving Pods.
			},
		}
		return "", framework.NewStatus(framework.Unschedulable, fitError.Error())
	}

	// 3) Interact with registered Extenders to filter out some candidates if needed.
	candidates, status = dp.CallExtenders(pl.fh.Extenders(), preemptor, nodeLister, candidates)
	if !status.IsSuccess() {
		return "", status
	}

	// 4) Find the best candidate.
	bestCandidate := dp.SelectCandidate(candidates)
	if bestCandidate == nil || len(bestCandidate.Name()) == 0 {
		return "", nil
	}

	// 5) Perform preparation work before nominating the selected candidate.
	if status := dp.PrepareCandidate(bestCandidate, pl.fh, cs, preemptor, pl.Name()); !status.IsSuccess() {
		return "", status
	}

	return bestCandidate.Name(), nil
}

// ExemptedFromPreemption evaluates whether the victimCandidate
// pod can tolerate from preemption by the preemptor pod or not
// by inspecting PriorityClass of victimCandidate pod.
// The function is public because other plugin can evaluate preemption toleration policy
// This would be useful other PostFilter plugin depends on the preemption toleration feature.
func ExemptedFromPreemption(
	victimCandidate, preemptor *v1.Pod,
	pcLister schedulinglisters.PriorityClassLister,
	now time.Time,
) (bool, error) {
	if victimCandidate.Spec.PriorityClassName == "" {
		return false, nil
	}
	victimPriorityClass, err := pcLister.Get(victimCandidate.Spec.PriorityClassName)
	if err != nil {
		return false, err
	}

	preemptorPreemptionPolicy := v1.PreemptLowerPriority
	if preemptor.Spec.PreemptionPolicy != nil {
		preemptorPreemptionPolicy = *preemptor.Spec.PreemptionPolicy
	}
	if preemptorPreemptionPolicy == v1.PreemptNever {
		return true, nil
	}

	// check it can tolerate the preemption in terms of priority value
	policy, err := parsePreemptionTolerationPolicy(*victimPriorityClass)
	if err != nil {
		// if any error raised, no toleration at all
		klog.ErrorS(err, "Failed to parse preemption toleration policy of victim candidate's priorityclass.  This victim candidate can't tolerate the preemption",
			"PreemptorPod", klog.KObj(preemptor),
			"VictimCandidatePod", klog.KObj(victimCandidate),
			"VictimCandidatePriorityClass", klog.KRef("", victimPriorityClass.Name),
		)
		return false, nil
	}
	preemptorPriority := corev1helpers.PodPriority(preemptor)
	if preemptorPriority >= policy.MinimumPreemptablePriority {
		return false, nil
	}

	if policy.TolerationSeconds < 0 {
		return true, nil
	}

	// check it can tolerate the preemption in terms of toleration seconds
	_, scheduledCondition := podutil.GetPodCondition(&victimCandidate.Status, v1.PodScheduled)
	if scheduledCondition == nil || scheduledCondition.Status != v1.ConditionTrue {
		return true, nil
	}
	scheduledAt := scheduledCondition.LastTransitionTime.Time
	tolerationDuration := time.Duration(policy.TolerationSeconds) * time.Second

	return scheduledAt.Add(tolerationDuration).After(now), nil
}

// FindCandidates calculates a slice of preemption candidates.
// Each candidate is executable to make the given <preemptor> pod schedulable.
// This is almost identical to DefaultPreemption plugin's one.
func (pl *PreemptionToleration) FindCandidates(
	ctx context.Context,
	state *framework.CycleState,
	preemptor *v1.Pod,
	m framework.NodeToStatusMap,
) ([]dp.Candidate, framework.NodeToStatusMap, *framework.Status) {
	allNodes, err := pl.fh.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, nil, framework.AsStatus(err)
	}
	if len(allNodes) == 0 {
		return nil, nil, framework.NewStatus(framework.Error, "no nodes available")
	}
	potentialNodes, unschedulableNodeStatus := nodesWherePreemptionMightHelp(allNodes, m)
	if len(potentialNodes) == 0 {
		klog.V(3).InfoS("Preemption will not help schedule pod on any node", "pod", klog.KObj(preemptor))
		// In this case, we should clean-up any existing nominated node name of the pod.
		if err := util.ClearNominatedNodeName(pl.fh.ClientSet(), preemptor); err != nil {
			klog.ErrorS(err, "cannot clear 'NominatedNodeName' field of pod", "pod", klog.KObj(preemptor))
			// We do not return as this error is not critical.
		}
		return nil, unschedulableNodeStatus, nil
	}

	pdbs, err := getPodDisruptionBudgets(pl.pdbLister)
	if err != nil {
		return nil, nil, framework.AsStatus(err)
	}

	offset, numCandidates := pl.getOffsetAndNumCandidates(int32(len(potentialNodes)))
	if klog.V(5).Enabled() {
		var sample []string
		for i := offset; i < offset+10 && i < int32(len(potentialNodes)); i++ {
			sample = append(sample, potentialNodes[i].Node().Name)
		}
		klog.InfoS("Selecting candidates from a pool of nodes", "potentialNodesCount", len(potentialNodes), "offset", offset, "sampleLength", len(sample), "sample", sample, "candidates", numCandidates)
	}
	candidates, nodeStatuses := pl.dryRunPreemption(ctx, pl.fh, state, preemptor, potentialNodes, pdbs, offset, numCandidates)
	for node, status := range unschedulableNodeStatus {
		nodeStatuses[node] = status
	}
	return candidates, nodeStatuses, nil
}

// dryRunPreemption simulates Preemption logic on <potentialNodes> in parallel,
// returns preemption candidates and a map indicating filtered nodes statuses.
// The number of candidates depends on the constraints defined in the plugin's args. In the returned list of
// candidates, ones that do not violate PDB are preferred over ones that do.
func (pl *PreemptionToleration) dryRunPreemption(ctx context.Context, fh framework.Handle,
	state *framework.CycleState, pod *v1.Pod, potentialNodes []*framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget, offset int32, numCandidates int32) ([]dp.Candidate, framework.NodeToStatusMap) {
	nonViolatingCandidates := newCandidateList(numCandidates)
	violatingCandidates := newCandidateList(numCandidates)
	parallelCtx, cancel := context.WithCancel(ctx)
	nodeStatuses := make(framework.NodeToStatusMap)
	var statusesLock sync.Mutex
	now := pl.clock.Now()
	checkNode := func(i int) {
		nodeInfoCopy := potentialNodes[(int(offset)+i)%len(potentialNodes)].Clone()
		stateCopy := state.Clone()
		pods, numPDBViolations, status := pl.selectVictimsOnNode(ctx, fh, stateCopy, pod, nodeInfoCopy, pdbs, now)
		if status.IsSuccess() {
			victims := extenderv1.Victims{
				Pods:             pods,
				NumPDBViolations: int64(numPDBViolations),
			}
			c := &candidate{
				victims: &victims,
				name:    nodeInfoCopy.Node().Name,
			}
			if numPDBViolations == 0 {
				nonViolatingCandidates.add(c)
			} else {
				violatingCandidates.add(c)
			}
			nvcSize, vcSize := nonViolatingCandidates.size(), violatingCandidates.size()
			if nvcSize > 0 && nvcSize+vcSize >= numCandidates {
				cancel()
			}
		} else {
			statusesLock.Lock()
			nodeStatuses[nodeInfoCopy.Node().Name] = status
			statusesLock.Unlock()
		}
	}
	fh.Parallelizer().Until(parallelCtx, len(potentialNodes), checkNode)
	return append(nonViolatingCandidates.get(), violatingCandidates.get()...), nodeStatuses
}

// selectVictimsOnNode finds minimum set of pods on the given node that should
// be preempted in order to make enough room for "preemptor" to be scheduled.
// The algorithm is almost identical to DefaultPreemption plugin's one.
// The only difference is that it takes PreemptionToleration annotations in
// PriorityClass resources into account for selecting victim pods.
func (pl *PreemptionToleration) selectVictimsOnNode(
	ctx context.Context,
	fh framework.Handle,
	state *framework.CycleState,
	preemptor *v1.Pod,
	nodeInfo *framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget,
	now time.Time,
) ([]*v1.Pod, int, *framework.Status) {
	var potentialVictims []*framework.PodInfo
	removePod := func(rpi *framework.PodInfo) error {
		if err := nodeInfo.RemovePod(rpi.Pod); err != nil {
			return err
		}
		status := fh.RunPreFilterExtensionRemovePod(ctx, state, preemptor, rpi, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}
	addPod := func(api *framework.PodInfo) error {
		nodeInfo.AddPodInfo(api)
		status := fh.RunPreFilterExtensionAddPod(ctx, state, preemptor, api, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}

	// As the first step, remove all lower priority pods that can't tolerate preemption by the preemptor
	// from the node and check if the given pod can be scheduled.
	podPriority := corev1helpers.PodPriority(preemptor)
	for _, pi := range nodeInfo.Pods {
		if corev1helpers.PodPriority(pi.Pod) >= podPriority {
			continue
		}

		// For a pod with lower priority, check if it can be exempted from the preemption.
		exempted, err := ExemptedFromPreemption(pi.Pod, preemptor, pl.priorityClassLister, now)
		if err != nil {
			klog.ErrorS(err, "Encountered error while selecting victims on node", "Node", nodeInfo.Node().Name)
			return nil, 0, framework.AsStatus(err)
		}

		if !exempted {
			potentialVictims = append(potentialVictims, pi)
			if err := removePod(pi); err != nil {
				return nil, 0, framework.AsStatus(err)
			}
		}
	}

	// No potential victims are found, and so we don't need to evaluate the node again since its state didn't change.
	if len(potentialVictims) == 0 {
		message := fmt.Sprintf("No victims found on node %v for preemptor pod %v", nodeInfo.Node().Name, preemptor.Name)
		return nil, 0, framework.NewStatus(framework.UnschedulableAndUnresolvable, message)
	}

	// If the new pod does not fit after removing all the lower priority pods,
	// we are almost done and this node is not suitable for preemption. The only
	// condition that we could check is if the "pod" is failing to schedule due to
	// inter-pod affinity to one or more victims, but we have decided not to
	// support this case for performance reasons. Having affinity to lower
	// priority pods is not a recommended configuration anyway.
	if status := fh.RunFilterPluginsWithNominatedPods(ctx, state, preemptor, nodeInfo); !status.IsSuccess() {
		return nil, 0, status
	}
	var victims []*v1.Pod
	numViolatingVictim := 0
	sort.Slice(potentialVictims, func(i, j int) bool { return util.MoreImportantPod(potentialVictims[i].Pod, potentialVictims[j].Pod) })
	// Try to reprieve as many pods as possible. We first try to reprieve the PDB
	// violating victims and then other non-violating ones. In both cases, we start
	// from the highest priority victims.
	violatingVictims, nonViolatingVictims := filterPodsWithPDBViolation(potentialVictims, pdbs)
	reprievePod := func(pi *framework.PodInfo) (bool, error) {
		if err := addPod(pi); err != nil {
			return false, err
		}
		status := fh.RunFilterPluginsWithNominatedPods(ctx, state, preemptor, nodeInfo)
		fits := status.IsSuccess()
		if !fits {
			if err := removePod(pi); err != nil {
				return false, err
			}
			rpi := pi.Pod
			victims = append(victims, rpi)
			klog.V(5).InfoS("Pod is a potential preemption victim on node", "pod", klog.KObj(rpi), "node", klog.KObj(nodeInfo.Node()))
		}
		return fits, nil
	}
	for _, p := range violatingVictims {
		if fits, err := reprievePod(p); err != nil {
			return nil, 0, framework.AsStatus(err)
		} else if !fits {
			numViolatingVictim++
		}
	}
	// Now we try to reprieve non-violating victims.
	for _, p := range nonViolatingVictims {
		if _, err := reprievePod(p); err != nil {
			return nil, 0, framework.AsStatus(err)
		}
	}
	return victims, numViolatingVictim, framework.NewStatus(framework.Success)
}
