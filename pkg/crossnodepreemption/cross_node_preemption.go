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

package crossnodepreemption

/*
import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	dp "k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
)

const (
	// Name of the plugin used in the plugin registry and configurations.
	Name = "CrossNodePreemption"
)

// CrossNodePreemption is a PostFilter plugin implements the preemption logic.
type CrossNodePreemption struct {
	fh        framework.Handle
	podLister corelisters.PodLister
}

var _ framework.PostFilterPlugin = &CrossNodePreemption{}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *CrossNodePreemption) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	pl := CrossNodePreemption{
		fh:        fh,
		podLister: fh.SharedInformerFactory().Core().V1().Pods().Lister(),
	}
	return &pl, nil
}

// PostFilter invoked at the postFilter extension point.
func (pl *CrossNodePreemption) PostFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
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

func (pl *CrossNodePreemption) preempt(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap) (string, *framework.Status) {
	cs := pl.fh.ClientSet()
	nodeLister := pl.fh.SnapshotSharedLister().NodeInfos()

	// Fetch the latest version of <pod>.
	// It's safe to directly fetch pod here. Because the informer cache has already been
	// initialized when creating the Scheduler obj, i.e., factory.go#MakeDefaultErrorFunc().
	// However, tests may need to manually initialize the shared pod informer.
	pod, err := pl.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		klog.ErrorS(err, "Error getting the updated preemptor", "pod", klog.KObj(pod))
		return "", framework.AsStatus(err)
	}

	// 1) Ensure the preemptor is eligible to preempt other pods.
	if !dp.PodEligibleToPreemptOthers(pod, nodeLister, m[pod.Status.NominatedNodeName]) {
		klog.V(5).InfoS("Pod is not eligible for more preemption", "pod", klog.KObj(pod))
		return "", nil
	}

	// 2) Find all preemption candidates.
	candidates, status := FindCandidates(ctx, state, pod, m, pl.fh, nodeLister)
	if !status.IsSuccess() {
		return "", status
	}

	// Return a FitError only when there are no candidates that fit the pod.
	if len(candidates) == 0 {
		fitError := &framework.FitError{
			Pod: pod,
		}
		return "", framework.NewStatus(framework.Unschedulable, fitError.Error())
	}

	// 3) Interact with registered Extenders to filter out some candidates if needed.
	candidates, status = dp.CallExtenders(pl.fh.Extenders(), pod, nodeLister, candidates)
	if !status.IsSuccess() {
		return "", status
	}

	// 4) Find the best candidate.
	bestCandidate := dp.SelectCandidate(candidates)
	if bestCandidate == nil || len(bestCandidate.Name()) == 0 {
		return "", nil
	}

	// 5) Perform preparation work before nominating the selected candidate.
	if status := dp.PrepareCandidate(bestCandidate, pl.fh, cs, pod, pl.Name()); !status.IsSuccess() {
		return "", status
	}

	return bestCandidate.Name(), nil
}

// FindCandidates calculates a slice of preemption candidates.
// Each candidate is executable to make the given <pod> schedulable.
func FindCandidates(ctx context.Context, state *framework.CycleState, pod *v1.Pod, m framework.NodeToStatusMap,
	fh framework.Handle, nodeLister framework.NodeInfoLister) ([]dp.Candidate, *framework.Status) {
	allNodes, err := nodeLister.List()
	if err != nil {
		return nil, framework.AsStatus(err)
	}
	if len(allNodes) == 0 {
		return nil, framework.NewStatus(framework.Error, "no nodes available")
	}

	potentialNodes := nodesWherePreemptionMightHelp(allNodes, m)

	// A brute-force algorithm to try ALL possible pod combinations.
	// CAVEAT: don't use this in production env.
	return bruteForceDryRunPreemption(ctx, fh, state, pod, potentialNodes, nodeLister), nil
}

func bruteForceDryRunPreemption(ctx context.Context, fh framework.Handle, state *framework.CycleState,
	pod *v1.Pod, potentialNodes []*framework.NodeInfo, nodeLister framework.NodeInfoLister) []dp.Candidate {
	// Loop over <potentialNodes> and collect the pods that has lower priority than <pod>.
	priority := corev1helpers.PodPriority(pod)
	var pods []*v1.Pod
	for _, node := range potentialNodes {
		for i := range node.Pods {
			p := node.Pods[i].Pod
			if corev1helpers.PodPriority(p) < priority {
				pods = append(pods, p)
			}
		}
	}

	var path []*v1.Pod
	var result []dp.Candidate
	// We have 2^len(pods) choices in total.
	f := func(_pods []*v1.Pod) []dp.Candidate {
		return dryRunOnePass(ctx, pod, _pods, nodeLister, fh, state)
	}
	// Pass a slice pointer (&result) so as to change its elements in dfs().
	dfs(pods, 0, path, &result, f)

	return result
}

func dfs(pods []*v1.Pod, i int, path []*v1.Pod, result *[]dp.Candidate, f dryRunFunc) {
	if i >= len(pods) {
		*result = append(*result, f(path)...)
		return
	}

	// Pick, or not pick current pod.
	dfs(pods, i+1, append(path, pods[i]), result, f)
	dfs(pods, i+1, path, result, f)
}

type dryRunFunc func([]*v1.Pod) []dp.Candidate

func dryRunOnePass(ctx context.Context, preemptor *v1.Pod, pods []*v1.Pod, nodeLister framework.NodeInfoLister,
	fh framework.Handle, state *framework.CycleState) []dp.Candidate {
	stateCopy := state.Clone()
	var nodeCopies []*framework.NodeInfo
	// Remove all victim pods.
	for i := range pods {
		pod := pods[i]
		nodeInfo, _ := nodeLister.Get(pod.Spec.NodeName)
		nodeCopy := nodeInfo.Clone()
		nodeCopies = append(nodeCopies, nodeCopy)
		nodeCopy.RemovePod(pod)
		pInfo := framework.NewPodInfo(pod)
		fh.RunPreFilterExtensionRemovePod(ctx, stateCopy, preemptor, pInfo, nodeCopy)
	}
	// See if all Filter plugins passed.
	// NOTE: a complete search space is ALL nodes, but that would be expensive.
	var candidates []dp.Candidate
	for _, nodeInfo := range nodeCopies {
		if s := fh.RunFilterPluginsWithNominatedPods(ctx, stateCopy, preemptor, nodeInfo); s.IsSuccess() {
			candidates = append(candidates, &candidate{victims: pods, name: nodeInfo.Node().Name})
		}
	}
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
*/
