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
	"math/rand"
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1"
	schedulinglisters "k8s.io/client-go/listers/scheduling/v1"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"

	kubefeatures "k8s.io/kubernetes/pkg/features"
	schedulerapisconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/validation"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	schedutil "sigs.k8s.io/scheduler-plugins/pkg/util"

	externalv1alpha1 "sigs.k8s.io/scheduler-plugins/pkg/generated/listers/scheduling/v1alpha1"
)

const (
	// Name of the plugin used in the plugin registry and configurations.
	Name = "PreemptionToleration"
)

var (
	_ framework.PostFilterPlugin = &PreemptionToleration{}
	_ preemption.Interface       = &PreemptionToleration{}
)

var preemptCache = make(map[string]int64)
var mu = &sync.RWMutex{}

// PreemptionToleration is a PostFilter plugin implements the preemption logic.
type PreemptionToleration struct {
	fh                  framework.Handle
	args                config.PreemptionTolerationArgs
	podLister           corelisters.PodLister
	pdbLister           policylisters.PodDisruptionBudgetLister
	pgLister            externalv1alpha1.PodGroupLister
	priorityClassLister schedulinglisters.PriorityClassLister
	client              *versioned.Clientset
	elasticQuotaLister  externalv1alpha1.ElasticQuotaLister

	clock   util.Clock
	curTime time.Time
}

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

	client, err := versioned.NewForConfig(fh.KubeConfig())
	if err != nil {
		return nil, err
	}
	schedSharedInformerFactory := schedinformer.NewSharedInformerFactory(client, 0)
	elasticQuotaLister := schedSharedInformerFactory.Scheduling().V1alpha1().ElasticQuotas().Lister()
	pgLister := schedSharedInformerFactory.Scheduling().V1alpha1().PodGroups().Lister()
	pl := PreemptionToleration{
		fh:                  fh,
		client:              client,
		elasticQuotaLister:  elasticQuotaLister,
		pgLister:            pgLister,
		args:                *args,
		podLister:           fh.SharedInformerFactory().Core().V1().Pods().Lister(),
		priorityClassLister: fh.SharedInformerFactory().Scheduling().V1().PriorityClasses().Lister(),
		pdbLister:           getPDBLister(fh.SharedInformerFactory()),
		clock:               util.RealClock{},
	}

	schedSharedInformerFactory.Start(nil)

	return &pl, nil
}

// PostFilter invoked at the postFilter extension point.
func (pl *PreemptionToleration) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	defer func() {
		metrics.PreemptionAttempts.Inc()
	}()

	pe := preemption.Evaluator{
		PluginName: pl.Name(),
		Handler:    pl.fh,
		PodLister:  pl.podLister,
		PdbLister:  pl.pdbLister,
		State:      state,
		Interface:  pl,
	}

	pl.curTime = pl.clock.Now()
	return pe.Preempt(ctx, pod, m)
}

// ExemptedFromPreemption evaluates whether the victimCandidate
// pod can tolerate from preemption by the preemptor pod or not
// by inspecting PriorityClass of victimCandidate pod.
// The function is public because other plugin can evaluate preemption toleration policy
// This would be useful other PostFilter plugin depends on the preemption toleration feature.
func ExemptedFromPreemption(
	victimCandidate, preemptor *v1.Pod,
	pcLister schedulinglisters.PriorityClassLister,
	pgLister externalv1alpha1.PodGroupLister,
	eqLister externalv1alpha1.ElasticQuotaLister,
	now time.Time,
) (bool, error) {

	// ########## Deny preemptions from other namespaces when max is full #############

	eqs, err := eqLister.ElasticQuotas(preemptor.Namespace).List(labels.NewSelector())
	if err != nil {
		return false, err
	}
	if len(eqs) == 1 {
		klog.V(5).InfoS("ElasticQuota Found", "namespace", preemptor.Namespace)
		eq := eqs[0]
		// Go over each configured max in an ElasticQuota to avoid checking resources that are shown
		// in used and does not have configured max.
		preemptorResources := schedutil.GetPodEffectiveRequest(preemptor)
		pgMembers := getPodGroupMembers(pgLister, preemptor)
		for resName, maxVal := range eq.Spec.Max {
			usedVal := eq.Status.Used[resName]
			preemptorReq := preemptorResources[resName]
			preemptorPGReq := preemptorReq.Value() * pgMembers

			// If we maxed out the usage of the ElasticQuota already, or the preemptor's pod group total request plus the used value will be over the max,
			// we want to avoid preempting resources from other namespaces. So if the victim doesn't belong to the preemptor namespace,
			// we exemapt it.
			//
			// We look at the pod group's total to make sure one pod out of many does not cause preemption, even tough the pod group
			// will be blocked by the EQ plugin later.
			if preemptorPGReq+usedVal.Value() > maxVal.Value() && preemptor.Namespace != victimCandidate.Namespace {
				klog.V(5).InfoS(
					"Victim exempted",
					"victim", victimCandidate.Name,
					"victim_namespace", victimCandidate.Namespace,
					"preemptor", preemptor.Name,
					"preemptor_namespace", preemptor.Namespace,
					"reason", "Used more than max in EQ",
				)
				return true, nil
			}
		}
	}
	// ####### END ###########

	preemptorPriorityClass, err := pcLister.Get(preemptor.Spec.PriorityClassName)
	if err != nil {
		return false, err
	}

	preemptorPreemptionPolicy := v1.PreemptLowerPriority
	if preemptor.Spec.PreemptionPolicy != nil {
		preemptorPreemptionPolicy = *preemptor.Spec.PreemptionPolicy
	}

	if preemptorPreemptionPolicy == v1.PreemptNever {
		klog.V(5).InfoS("Victim exempted", "reason", "Preemptor policy set to Never")
		return true, nil
	}

	if victimCandidate.Spec.PriorityClassName == "" {
		return false, nil
	}

	// Since we handled victims with no priority classes above, it is not safe to collect data.
	victimPriorityClass, err := pcLister.Get(victimCandidate.Spec.PriorityClassName)
	if err != nil {
		if !errors.IsNotFound(err) {
			return false, err
		}
	}

	vicPolicy, err := parsePreemptionTolerationPolicy(*victimPriorityClass)
	if err != nil {
		// if any error raised, no toleration at all
		klog.ErrorS(err, "Failed to parse preemption toleration policy of victim candidate's priorityclass.  This victim candidate can't tolerate the preemption",
			"PreemptorPod", klog.KObj(preemptor),
			"VictimCandidatePod", klog.KObj(victimCandidate),
			"VictimCandidatePriorityClass", klog.KRef("", victimPriorityClass.Name),
		)
		return false, nil
	}

	if preemptorPriorityClass.Value <= vicPolicy.MinimumPreemptablePriority {
		klog.V(5).InfoS(
			"Victim exempted",
			"reason", "Preemptor priority value is less-equal to the minimum priority configured",
			"preemptor", preemptor.Name,
			"victim", victimCandidate.Name,
		)
		return true, nil
	}

	klog.V(5).InfoS(
		"Victim not exempted",
		"reason", "Victim did not match any of the filters",
		"preemptor", preemptor.Name,
		"victim", victimCandidate.Name,
	)
	return false, nil

}

func getPodGroupMembers(pgLister externalv1alpha1.PodGroupLister, pod *v1.Pod) int64 {
	pgName := schedutil.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return 1
	}
	pg, err := pgLister.PodGroups(pod.Namespace).Get(pgName)
	if err != nil {
		return 1
	}
	return int64(pg.Spec.MinMember)
}

// SelectVictimsOnNode finds minimum set of pods on the given node that should
// be preempted in order to make enough room for "preemptor" to be scheduled.
// The algorithm is almost identical to DefaultPreemption plugin's one.
// The only difference is that it takes PreemptionToleration annotations in
// PriorityClass resources into account for selecting victim pods.
func (pl *PreemptionToleration) SelectVictimsOnNode(
	ctx context.Context,
	state *framework.CycleState,
	preemptor *v1.Pod,
	nodeInfo *framework.NodeInfo,
	pdbs []*policy.PodDisruptionBudget) ([]*v1.Pod, int, *framework.Status) {
	var potentialVictims []*framework.PodInfo
	removePod := func(rpi *framework.PodInfo) error {
		if err := nodeInfo.RemovePod(rpi.Pod); err != nil {
			return err
		}
		status := pl.fh.RunPreFilterExtensionRemovePod(ctx, state, preemptor, rpi, nodeInfo)
		if !status.IsSuccess() {
			return status.AsError()
		}
		return nil
	}
	addPod := func(api *framework.PodInfo) error {
		nodeInfo.AddPodInfo(api)
		status := pl.fh.RunPreFilterExtensionAddPod(ctx, state, preemptor, api, nodeInfo)
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
		exempted, err := ExemptedFromPreemption(pi.Pod, preemptor, pl.priorityClassLister, pl.pgLister, pl.elasticQuotaLister, pl.curTime)
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
	if status := pl.fh.RunFilterPluginsWithNominatedPods(ctx, state, preemptor, nodeInfo); !status.IsSuccess() {
		return nil, 0, status
	}
	var victims []*v1.Pod
	numViolatingVictim := 0

	sort.Slice(potentialVictims, func(i, j int) bool {
		return util.MoreImportantPod(potentialVictims[i].Pod, potentialVictims[j].Pod)

	})

	if klog.V(5).Enabled() {
		for i, v := range potentialVictims {
			klog.InfoS("potential victim", "number", i, "pod", v.Pod.Name, "priorityClassName", v.Pod.Spec.PriorityClassName)
		}
	}

	// Try to reprieve as many pods as possible. We first try to reprieve the PDB
	// violating victims and then other non-violating ones. In both cases, we start
	// from the highest priority victims.
	violatingVictims, nonViolatingVictims := filterPodsWithPDBViolation(potentialVictims, pdbs)
	reprievePod := func(pi *framework.PodInfo) (bool, error) {
		if err := addPod(pi); err != nil {
			return false, err
		}
		status := pl.fh.RunFilterPluginsWithNominatedPods(ctx, state, preemptor, nodeInfo)
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

func ReadCache(key string) int64 {
	mu.RLock()
	defer mu.RUnlock()
	return preemptCache[key]
}

func AddToCache(key string) {
	mu.Lock()
	defer mu.Unlock()

	preemptCache[key] = preemptCache[key] + 1
}

func DeleteFromCache(key string) {
	mu.Lock()
	defer mu.Unlock()
	delete(preemptCache, key)
}

/* DO NOT EDIT CONTENT BELOW */
/* Copied from k/k#pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go */

// GetOffsetAndNumCandidates chooses a random offset and calculates the number
// of candidates that should be shortlisted for dry running preemption.
func (pl *PreemptionToleration) GetOffsetAndNumCandidates(numNodes int32) (int32, int32) {
	return rand.Int31n(numNodes), pl.calculateNumCandidates(numNodes)
}

func (pl *PreemptionToleration) CandidatesToVictimsMap(candidates []preemption.Candidate) map[string]*extenderv1.Victims {
	m := make(map[string]*extenderv1.Victims)
	for _, c := range candidates {
		m[c.Name()] = c.Victims()
	}
	return m
}

// calculateNumCandidates returns the number of candidates the FindCandidates
// method must produce from dry running based on the constraints given by
// <minCandidateNodesPercentage> and <minCandidateNodesAbsolute>. The number of
// candidates returned will never be greater than <numNodes>.
func (pl *PreemptionToleration) calculateNumCandidates(numNodes int32) int32 {
	n := (numNodes * pl.args.MinCandidateNodesPercentage) / 100
	if n < pl.args.MinCandidateNodesAbsolute {
		n = pl.args.MinCandidateNodesAbsolute
	}
	if n > numNodes {
		n = numNodes
	}
	return n
}

// PodEligibleToPreemptOthers determines whether this pod should be considered
// for preempting other pods or not. If this pod has already preempted other
// pods and those are in their graceful termination period, it shouldn't be
// considered for preemption.
// We look at the node that is nominated for this pod and as long as there are
// terminating pods on the node, we don't consider this for preempting more pods.
func (pl *PreemptionToleration) PodEligibleToPreemptOthers(pod *v1.Pod, nominatedNodeStatus *framework.Status) bool {
	if pod.Spec.PreemptionPolicy != nil && *pod.Spec.PreemptionPolicy == v1.PreemptNever {
		klog.V(5).InfoS("Pod is not eligible for preemption because it has a preemptionPolicy of Never", "pod", klog.KObj(pod))
		return false
	}
	nodeInfos := pl.fh.SnapshotSharedLister().NodeInfos()
	nomNodeName := pod.Status.NominatedNodeName
	if len(nomNodeName) > 0 {
		// If the pod's nominated node is considered as UnschedulableAndUnresolvable by the filters,
		// then the pod should be considered for preempting again.
		if nominatedNodeStatus.Code() == framework.UnschedulableAndUnresolvable {
			return true
		}

		if nodeInfo, _ := nodeInfos.Get(nomNodeName); nodeInfo != nil {
			podPriority := corev1helpers.PodPriority(pod)
			for _, p := range nodeInfo.Pods {
				if p.Pod.DeletionTimestamp != nil && corev1helpers.PodPriority(p.Pod) < podPriority {
					// There is a terminating pod on the nominated node.
					return false
				}
			}
		}
	}
	return true
}

// filterPodsWithPDBViolation groups the given "pods" into two groups of "violatingPods"
// and "nonViolatingPods" based on whether their PDBs will be violated if they are
// preempted.
// This function is stable and does not change the order of received pods. So, if it
// receives a sorted list, grouping will preserve the order of the input list.
func filterPodsWithPDBViolation(podInfos []*framework.PodInfo, pdbs []*policy.PodDisruptionBudget) (violatingPodInfos, nonViolatingPodInfos []*framework.PodInfo) {
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
			violatingPodInfos = append(violatingPodInfos, podInfo)
		} else {
			nonViolatingPodInfos = append(nonViolatingPodInfos, podInfo)
		}
	}
	return violatingPodInfos, nonViolatingPodInfos
}

func getPDBLister(informerFactory informers.SharedInformerFactory) policylisters.PodDisruptionBudgetLister {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodDisruptionBudget) {
		return informerFactory.Policy().V1().PodDisruptionBudgets().Lister()
	}
	return nil
}
