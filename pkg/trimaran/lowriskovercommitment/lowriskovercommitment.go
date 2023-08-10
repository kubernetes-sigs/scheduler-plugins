/*
Copyright 2023 The Kubernetes Authors.

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

// Package loadvariationriskbalancing plugin attempts to balance the risk in load variation
// across the cluster. Risk is expressed as the sum of average and standard deviation of
// the measured load on a node. Typical load balancing involves only the average load.
// Here, we consider the variation in load as well, hence resulting in a safer balance.

package lowriskovercommitment

import (
	"context"
	"fmt"
	"math"

	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	pluginv1 "sigs.k8s.io/scheduler-plugins/apis/config/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	// Name : name of plugin
	Name = "LowRiskOverCommitment"

	// MaxVarianceAllowance : allowed value from the maximum variance (to avoid zero divisions)
	MaxVarianceAllowance = 0.99

	// State key used in CycleState
	PodResourcesKey = Name + ".PodResources"
)

// LowRiskOverCommitment : scheduler plugin
type LowRiskOverCommitment struct {
	handle              framework.Handle
	collector           *trimaran.Collector
	args                *pluginConfig.LowRiskOverCommitmentArgs
	riskLimitWeightsMap map[v1.ResourceName]float64
}

// New : create an instance of a LowRiskOverCommitment plugin
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(4).InfoS("Creating new instance of the LowRiskOverCommitment plugin")
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.LowRiskOverCommitmentArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type LowRiskOverCommitmentArgs, got %T", obj)
	}
	collector, err := trimaran.NewCollector(&args.TrimaranSpec)
	if err != nil {
		return nil, err
	}
	// create map of resource risk limit weights
	m := make(map[v1.ResourceName]float64)
	m[v1.ResourceCPU] = pluginv1.DefaultRiskLimitWeight
	m[v1.ResourceMemory] = pluginv1.DefaultRiskLimitWeight
	for r, w := range args.RiskLimitWeights {
		m[r] = w
	}
	klog.V(4).InfoS("Using LowRiskOverCommitmentArgs", "smoothingWindowSize", args.SmoothingWindowSize,
		"riskLimitWeights", m)

	pl := &LowRiskOverCommitment{
		handle:              handle,
		collector:           collector,
		args:                args,
		riskLimitWeightsMap: m,
	}
	return pl, nil
}

// PreScore : calculate pod requests and limits and store as plugin state data to be used during scoring
func (pl *LowRiskOverCommitment) PreScore(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodes []*v1.Node) *framework.Status {
	klog.V(6).InfoS("PreScore: Calculating pod resource requests and limits", "pod", klog.KObj(pod))
	podResourcesStateData := CreatePodResourcesStateData(pod)
	cycleState.Write(PodResourcesKey, podResourcesStateData)
	return nil
}

// Score : evaluate score for a node
func (pl *LowRiskOverCommitment) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	klog.V(6).InfoS("Score: Calculating score", "pod", klog.KObj(pod), "nodeName", nodeName)
	score := framework.MinNodeScore

	defer func() {
		klog.V(6).InfoS("Calculating totalScore", "pod", klog.KObj(pod), "nodeName", nodeName, "totalScore", score)
	}()

	// get pod requests and limits
	podResources, err := getPreScoreState(cycleState)
	if err != nil {
		// calculate pod requests and limits, if missing
		klog.V(6).InfoS(err.Error()+"; recalculating", "pod", klog.KObj(pod))
		podResources = CreatePodResourcesStateData(pod)
	}
	// exclude scoring for best effort pods; this plugin is not concerned about best effort pods
	podRequests := &podResources.podRequests
	podLimits := &podResources.podLimits
	if podRequests.MilliCPU == 0 && podRequests.Memory == 0 &&
		podLimits.MilliCPU == 0 && podLimits.Memory == 0 {
		klog.V(6).InfoS("Skipping scoring best effort pod; using minimum score", "nodeName", nodeName, "pod", klog.KObj(pod))
		return score, nil
	}
	// get node info
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	// get node metrics
	metrics, _ := pl.collector.GetNodeMetrics(nodeName)
	if metrics == nil {
		klog.InfoS("Failed to get metrics for node; using minimum score", "nodeName", nodeName)
		return score, nil
	}
	// calculate score
	totalScore := pl.computeRank(metrics, nodeInfo, pod, podRequests, podLimits) * float64(framework.MaxNodeScore)
	score = int64(math.Round(totalScore))
	return score, framework.NewStatus(framework.Success, "")
}

// Name : name of plugin
func (pl *LowRiskOverCommitment) Name() string {
	return Name
}

// ScoreExtensions : an interface for Score extended functionality
func (pl *LowRiskOverCommitment) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// NormalizeScore : normalize scores
func (pl *LowRiskOverCommitment) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// computeRank : rank function for the LowRiskOverCommitment
func (pl *LowRiskOverCommitment) computeRank(metrics []watcher.Metric, nodeInfo *framework.NodeInfo, pod *v1.Pod,
	podRequests *framework.Resource, podLimits *framework.Resource) float64 {
	node := nodeInfo.Node()
	// calculate risk based on requests and limits
	nodeRequestsAndLimits := trimaran.GetNodeRequestsAndLimits(nodeInfo.Pods, node, pod, podRequests, podLimits)
	riskCPU := pl.computeRisk(metrics, v1.ResourceCPU, watcher.CPU, node, nodeRequestsAndLimits)
	riskMemory := pl.computeRisk(metrics, v1.ResourceMemory, watcher.Memory, node, nodeRequestsAndLimits)
	rank := 1 - math.Max(riskCPU, riskMemory)

	klog.V(6).InfoS("Node rank", "nodeName", node.GetName(), "riskCPU", riskCPU, "riskMemory", riskMemory, "rank", rank)

	return rank
}

// computeRisk : calculate the risk of scheduling on node for a given resource
func (pl *LowRiskOverCommitment) computeRisk(metrics []watcher.Metric, resourceName v1.ResourceName,
	resourceType string, node *v1.Node, nodeRequestsAndLimits *trimaran.NodeRequestsAndLimits) float64 {
	var riskLimit, riskLoad, totalRisk float64

	defer func() {
		klog.V(6).InfoS("Calculated risk", "node", klog.KObj(node), "resource", resourceName,
			"riskLimit", riskLimit, "riskLoad", riskLoad, "totalRisk", totalRisk)
	}()

	nodeRequest := nodeRequestsAndLimits.NodeRequest
	nodeLimit := nodeRequestsAndLimits.NodeLimit
	nodeRequestMinusPod := nodeRequestsAndLimits.NodeRequestMinusPod
	nodeLimitMinusPod := nodeRequestsAndLimits.NodeLimitMinusPod
	nodeCapacity := nodeRequestsAndLimits.Nodecapacity

	var request, limit, capacity, requestMinusPod, limitMinusPod int64
	if resourceName == v1.ResourceCPU {
		request = nodeRequest.MilliCPU
		limit = nodeLimit.MilliCPU
		requestMinusPod = nodeRequestMinusPod.MilliCPU
		limitMinusPod = nodeLimitMinusPod.MilliCPU
		capacity = nodeCapacity.MilliCPU
	} else if resourceName == v1.ResourceMemory {
		request = nodeRequest.Memory
		limit = nodeLimit.Memory
		requestMinusPod = nodeRequestMinusPod.Memory
		limitMinusPod = nodeLimitMinusPod.Memory
		capacity = nodeCapacity.Memory
	} else {
		// invalid resource
		klog.V(6).InfoS("Unexpected resource", "resourceName", resourceName)
		return 0
	}

	// (1) riskLimit : calculate overcommit potential load
	if limit > capacity {
		riskLimit = float64(limit-capacity) / float64(limit-request)
	}
	klog.V(6).InfoS("RiskLimit", "node", klog.KObj(node), "resource", resourceName, "riskLimit", riskLimit)

	// (2) riskLoad : calculate measured overcommitment
	zeroRequest := &framework.Resource{}
	stats, ok := trimaran.CreateResourceStats(metrics, node, zeroRequest, resourceName, resourceType)
	if ok {
		// fit a beta distribution to the measured load stats
		mu, sigma := trimaran.GetMuSigma(stats)
		// adjust standard deviation due to data smoothing
		sigma *= math.Pow(float64(pl.args.SmoothingWindowSize), 0.5)
		// limit the standard deviation close to the allowed maximum for the beta distribution
		sigma = math.Min(sigma, math.Sqrt(GetMaxVariance(mu)*MaxVarianceAllowance))

		// calculate area under beta probability curve beyond total allocated, as overuse risk measure
		allocThreshold := float64(requestMinusPod) / float64(capacity)
		allocThreshold = math.Min(math.Max(allocThreshold, 0), 1)
		allocProb, fitDistribution := ComputeProbability(mu, sigma, allocThreshold)
		if fitDistribution != nil {
			klog.V(6).InfoS("FitDistribution", "node", klog.KObj(node), "resource", resourceName, "dist", fitDistribution.Print())
		}
		// condition the probability in case total limit is less than capacity
		if limitMinusPod < capacity && requestMinusPod <= limitMinusPod {
			limitThreshold := float64(limitMinusPod) / float64(capacity)
			if limitThreshold == 0 {
				allocProb = 1 // zero over zero
			} else if fitDistribution != nil {
				limitProb := fitDistribution.DistributionFunction(limitThreshold)
				if limitProb > 0 {
					allocProb /= limitProb
					allocProb = math.Min(math.Max(allocProb, 0), 1)
				}
			}
		}

		// calculate risk
		riskLoad = 1 - allocProb
		klog.V(6).InfoS("RiskLoad", "node", klog.KObj(node), "resource", resourceName,
			"allocThreshold", allocThreshold, "allocProb", allocProb, "riskLoad", riskLoad)
	}

	// combine two components of risk into a total risk as a weighted sum
	w := pl.riskLimitWeightsMap[resourceName]
	totalRisk = w*riskLimit + (1-w)*riskLoad
	totalRisk = math.Min(math.Max(totalRisk, 0), 1)
	return totalRisk
}

// CreatePodResourcesStateData : calculate pod resource requests and limits and store as plugin state data
func CreatePodResourcesStateData(pod *v1.Pod) *PodResourcesStateData {
	requests := trimaran.GetResourceRequested(pod)
	limits := trimaran.GetResourceLimits(pod)
	// make sure limits not less than requests
	trimaran.SetMaxLimits(requests, limits)
	return &PodResourcesStateData{
		podRequests: *requests,
		podLimits:   *limits,
	}
}

// PodResourcesStateData : computed at PreScore and used at Score
type PodResourcesStateData struct {
	podRequests framework.Resource
	podLimits   framework.Resource
}

// Clone : clone the pod resource state data
func (s *PodResourcesStateData) Clone() framework.StateData {
	return s
}

// getPreScoreState: retrieve pod requests and limits from plugin state data
func getPreScoreState(cycleState *framework.CycleState) (*PodResourcesStateData, error) {
	podResourcesStateData, err := cycleState.Read(PodResourcesKey)
	if err != nil {
		return nil, fmt.Errorf("reading %q from cycleState: %w", PodResourcesKey, err)
	}
	podResources, ok := podResourcesStateData.(*PodResourcesStateData)
	if !ok {
		return nil, fmt.Errorf("invalid PreScore state, got type %T", podResourcesStateData)
	}
	return podResources, nil
}
