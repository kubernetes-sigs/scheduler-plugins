/*
Copyright 2022 The Kubernetes Authors.

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

package networkoverhead

import (
	"context"
	"fmt"
	"math"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/controller-runtime/pkg/client"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
)

var _ framework.PreFilterPlugin = &NetworkOverhead{}
var _ framework.FilterPlugin = &NetworkOverhead{}
var _ framework.ScorePlugin = &NetworkOverhead{}

const (
	// Name : name of plugin used in the plugin registry and configurations.
	Name = "NetworkOverhead"

	// MaxCost : MaxCost used in the NetworkTopology for costs between origins and destinations
	MaxCost = 100

	// SameHostname : If pods belong to the same host, then consider cost as 0
	SameHostname = 0

	// SameZone : If pods belong to hosts in the same zone, then consider cost as 1
	SameZone = 1

	// preFilterStateKey is the key in CycleState to NetworkOverhead pre-computed data.
	preFilterStateKey = "PreFilter" + Name
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(agv1alpha1.AddToScheme(scheme))
	utilruntime.Must(ntv1alpha1.AddToScheme(scheme))
}

// NetworkOverhead : Filter and Score nodes based on Pod's AppGroup requirements: MaxNetworkCosts requirements among Pods with dependencies
type NetworkOverhead struct {
	client.Client
	logger      klog.Logger
	podLister   corelisters.PodLister
	handle      framework.Handle
	namespaces  []string
	weightsName string
	ntName      string
}

// PreFilterState computed at PreFilter and used at Filter and Score.
type PreFilterState struct {
	// boolean that tells the filter and scoring functions to pass the pod since it does not belong to an AppGroup
	scoreEqually bool

	// AppGroup name of the pod
	agName string

	// AppGroup CR
	appGroup *agv1alpha1.AppGroup

	// NetworkTopology CR
	networkTopology *ntv1alpha1.NetworkTopology

	// Dependency List of the given pod
	dependencyList []agv1alpha1.DependenciesInfo

	// Pods already scheduled based on the dependency list
	scheduledList networkawareutil.ScheduledList

	// node map for cost / destinations. Search for requirements faster...
	nodeCostMap map[string]map[networkawareutil.CostKey]int64

	// node map for satisfied dependencies
	satisfiedMap map[string]int64

	// node map for violated dependencies
	violatedMap map[string]int64

	// node map for costs
	finalCostMap map[string]int64
}

// Clone the preFilter state.
func (no *PreFilterState) Clone() framework.StateData {
	return no
}

// Name : returns name of the plugin.
func (no *NetworkOverhead) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (*pluginconfig.NetworkOverheadArgs, error) {
	NetworkOverheadArgs, ok := obj.(*pluginconfig.NetworkOverheadArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NetworkOverhead, got %T", obj)
	}

	return NetworkOverheadArgs, nil
}

// ScoreExtensions : an interface for Score extended functionality
func (no *NetworkOverhead) ScoreExtensions() framework.ScoreExtensions {
	return no
}

// New : create an instance of a NetworkOverhead plugin
func New(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)
	logger.V(4).Info("Creating new instance of the NetworkOverhead plugin")

	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	client, err := client.New(handle.KubeConfig(), client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}

	no := &NetworkOverhead{
		Client:      client,
		logger:      logger,
		podLister:   handle.SharedInformerFactory().Core().V1().Pods().Lister(),
		handle:      handle,
		namespaces:  args.Namespaces,
		weightsName: args.WeightsName,
		ntName:      args.NetworkTopologyName,
	}
	return no, nil
}

// PreFilter performs the following operations:
// 1. Get appGroup name and respective appGroup CR.
// 2. Get networkTopology CR.
// 3. Get dependency and scheduled list for the given pod
// 4. Update cost map of all nodes
// 5. Get number of satisfied and violated dependencies
// 6. Get final cost of the given node to be used in the score plugin
func (no *NetworkOverhead) PreFilter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod) (*framework.PreFilterResult, *framework.Status) {
	// Init PreFilter State
	preFilterState := &PreFilterState{
		scoreEqually: true,
	}
	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "PreFilter")

	// Write initial status
	state.Write(preFilterStateKey, preFilterState)

	// Check if Pod belongs to an AppGroup
	agName := networkawareutil.GetPodAppGroupLabel(pod)
	if len(agName) == 0 { // Return
		return nil, framework.NewStatus(framework.Success, "Pod does not belong to an AppGroup, return")
	}

	// Get AppGroup CR
	appGroup := no.findAppGroupNetworkOverhead(ctx, agName)

	// Get NetworkTopology CR
	networkTopology := no.findNetworkTopologyNetworkOverhead(ctx)

	// Sort Costs if manual weights were selected
	no.sortNetworkTopologyCosts(networkTopology)

	// Get Dependencies of the given pod
	dependencyList := networkawareutil.GetDependencyList(pod, appGroup)

	// If the pod has no dependencies, return
	if dependencyList == nil {
		return nil, framework.NewStatus(framework.Success, "Pod has no dependencies, return")
	}

	// Get pods from lister
	selector := labels.Set(map[string]string{agv1alpha1.AppGroupLabel: agName}).AsSelector()
	pods, err := no.podLister.List(selector)
	if err != nil {
		return nil, framework.NewStatus(framework.Success, "Error while returning pods from appGroup, return")
	}

	// Return if pods are not yet allocated for the AppGroup...
	if len(pods) == 0 {
		return nil, framework.NewStatus(framework.Success, "No pods yet allocated, return")
	}

	// Pods already scheduled: Get Scheduled List (Deployment name, replicaID, hostname)
	scheduledList := networkawareutil.GetScheduledList(pods)
	// Check if scheduledList is empty...
	if len(scheduledList) == 0 {
		logger.Error(nil, "Scheduled list is empty, return")
		return nil, framework.NewStatus(framework.Success, "Scheduled list is empty, return")
	}

	// Get all nodes
	nodeList, err := no.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, fmt.Sprintf("Error getting the nodelist: %v", err))
	}

	// Create variables to fill PreFilterState
	nodeCostMap := make(map[string]map[networkawareutil.CostKey]int64)
	satisfiedMap := make(map[string]int64)
	violatedMap := make(map[string]int64)
	finalCostMap := make(map[string]int64)

	// For each node:
	// 1 - Get region and zone labels
	// 2 - Calculate satisfied and violated number of dependencies
	// 3 - Calculate the final cost of the node to be used by the scoring plugin
	for _, nodeInfo := range nodeList {
		// retrieve region and zone labels
		region := networkawareutil.GetNodeRegion(nodeInfo.Node())
		zone := networkawareutil.GetNodeZone(nodeInfo.Node())
		logger.V(6).Info("Node info",
			"name", nodeInfo.Node().Name,
			"region", region,
			"zone", zone)

		// Create map for cost / destinations. Search for requirements faster...
		costMap := make(map[networkawareutil.CostKey]int64)

		// Populate cost map for the given node
		no.populateCostMap(costMap, networkTopology, region, zone)
		logger.V(6).Info("Map", "costMap", costMap)

		// Update nodeCostMap
		nodeCostMap[nodeInfo.Node().Name] = costMap

		// Get Satisfied and Violated number of dependencies
		satisfied, violated, ok := checkMaxNetworkCostRequirements(logger, scheduledList, dependencyList, nodeInfo, region, zone, costMap, no)
		if ok != nil {
			return nil, framework.NewStatus(framework.Error, fmt.Sprintf("pod hostname not found: %v", ok))
		}

		// Update Satisfied and Violated maps
		satisfiedMap[nodeInfo.Node().Name] = satisfied
		violatedMap[nodeInfo.Node().Name] = violated
		logger.V(6).Info("Number of dependencies", "satisfied", satisfied, "violated", violated)

		// Get accumulated cost based on pod dependencies
		cost, ok := no.getAccumulatedCost(scheduledList, dependencyList, nodeInfo.Node().Name, region, zone, costMap)
		if ok != nil {
			return nil, framework.NewStatus(framework.Error, fmt.Sprintf("getting pod hostname from Snapshot: %v", ok))
		}
		logger.V(6).Info("Node final cost", "cost", cost)
		finalCostMap[nodeInfo.Node().Name] = cost
	}

	// Update PreFilter State
	preFilterState = &PreFilterState{
		scoreEqually:    false,
		agName:          agName,
		appGroup:        appGroup,
		networkTopology: networkTopology,
		dependencyList:  dependencyList,
		scheduledList:   scheduledList,
		nodeCostMap:     nodeCostMap,
		satisfiedMap:    satisfiedMap,
		violatedMap:     violatedMap,
		finalCostMap:    finalCostMap,
	}

	state.Write(preFilterStateKey, preFilterState)
	return nil, framework.NewStatus(framework.Success, "PreFilter State updated")
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (no *NetworkOverhead) PreFilterExtensions() framework.PreFilterExtensions {
	return no
}

// AddPod from pre-computed data in cycleState.
// no current need for the NetworkOverhead plugin
func (no *NetworkOverhead) AddPod(ctx context.Context,
	cycleState *framework.CycleState,
	podToSchedule *corev1.Pod,
	podToAdd *framework.PodInfo,
	nodeInfo *framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Success, "")
}

// RemovePod from pre-computed data in cycleState.
// no current need for the NetworkOverhead plugin
func (no *NetworkOverhead) RemovePod(ctx context.Context,
	cycleState *framework.CycleState,
	podToSchedule *corev1.Pod,
	podToRemove *framework.PodInfo,
	nodeInfo *framework.NodeInfo) *framework.Status {
	return framework.NewStatus(framework.Success, "")
}

// Filter : evaluate if node can respect maxNetworkCost requirements
func (no *NetworkOverhead) Filter(ctx context.Context,
	cycleState *framework.CycleState,
	pod *corev1.Pod,
	nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "Filter")

	// Get PreFilterState
	preFilterState, err := getPreFilterState(cycleState)
	if err != nil {
		logger.Error(err, "Failed to read preFilterState from cycleState", "preFilterStateKey", preFilterStateKey)
		return framework.NewStatus(framework.Error, "not eligible due to failed to read from cycleState")
	}

	// If scoreEqually, return nil
	if preFilterState.scoreEqually {
		logger.V(6).Info("Score all nodes equally, return")
		return nil
	}

	// Get satisfied and violated number of dependencies
	satisfied := preFilterState.satisfiedMap[nodeInfo.Node().Name]
	violated := preFilterState.violatedMap[nodeInfo.Node().Name]
	logger.V(6).Info("Number of dependencies:", "satisfied", satisfied, "violated", violated)

	// The pod is filtered out if the number of violated dependencies is higher than the satisfied ones
	if violated > satisfied {
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Node %v does not meet several network requirements from Workload dependencies: Satisfied: %v Violated: %v", nodeInfo.Node().Name, satisfied, violated))
	}
	return nil
}

// Score : evaluate score for a node
func (no *NetworkOverhead) Score(ctx context.Context,
	cycleState *framework.CycleState,
	pod *corev1.Pod,
	nodeName string) (int64, *framework.Status) {
	score := framework.MinNodeScore

	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "Score")
	// Get PreFilterState
	preFilterState, err := getPreFilterState(cycleState)
	if err != nil {
		logger.Error(err, "Failed to read preFilterState from cycleState", "preFilterStateKey", preFilterStateKey)
		return score, framework.NewStatus(framework.Error, "not eligible due to failed to read from cycleState, return min score")
	}

	// If scoreEqually, return minScore
	if preFilterState.scoreEqually {
		return score, framework.NewStatus(framework.Success, "scoreEqually enabled: minimum score")
	}

	// Return Accumulated Cost as score
	score = preFilterState.finalCostMap[nodeName]
	logger.V(4).Info("Score:", "pod", pod.GetName(), "node", nodeName, "finalScore", score)
	return score, framework.NewStatus(framework.Success, "Accumulated cost added as score, normalization ensures lower costs are favored")
}

// NormalizeScore : normalize scores since lower scores correspond to lower latency
func (no *NetworkOverhead) NormalizeScore(ctx context.Context,
	state *framework.CycleState,
	pod *corev1.Pod,
	scores framework.NodeScoreList) *framework.Status {
	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "NormalizeScore")
	logger.V(4).Info("before normalization: ", "scores", scores)

	// Get Min and Max Scores to normalize between framework.MaxNodeScore and framework.MinNodeScore
	minCost, maxCost := getMinMaxScores(scores)

	// If all nodes were given the minimum score, return
	if minCost == 0 && maxCost == 0 {
		return nil
	}

	var normCost float64
	for i := range scores {
		if maxCost != minCost { // If max != min
			// node_normalized_cost = MAX_SCORE * ( ( nodeScore - minCost) / (maxCost - minCost)
			// nodeScore = MAX_SCORE - node_normalized_cost
			normCost = float64(framework.MaxNodeScore) * float64(scores[i].Score-minCost) / float64(maxCost-minCost)
			scores[i].Score = framework.MaxNodeScore - int64(normCost)
		} else { // If maxCost = minCost, avoid division by 0
			normCost = float64(scores[i].Score - minCost)
			scores[i].Score = framework.MaxNodeScore - int64(normCost)
		}
	}
	logger.V(4).Info("after normalization: ", "scores", scores)
	return nil
}

// MinMax : get min and max scores from NodeScoreList
func getMinMaxScores(scores framework.NodeScoreList) (int64, int64) {
	var max int64 = math.MinInt64 // Set to min value
	var min int64 = math.MaxInt64 // Set to max value

	for _, nodeScore := range scores {
		if nodeScore.Score > max {
			max = nodeScore.Score
		}
		if nodeScore.Score < min {
			min = nodeScore.Score
		}
	}
	// return min and max scores
	return min, max
}

// sortNetworkTopologyCosts : sort costs if manual weights were selected
func (no *NetworkOverhead) sortNetworkTopologyCosts(networkTopology *ntv1alpha1.NetworkTopology) {
	if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts { // Manual weights were selected
		for _, w := range networkTopology.Spec.Weights {
			// Sort Costs by TopologyKey, might not be sorted since were manually defined
			sort.Sort(networkawareutil.ByTopologyKey(w.TopologyList))
		}
	}
}

// populateCostMap : Populates costMap based on the node being filtered/scored
func (no *NetworkOverhead) populateCostMap(
	costMap map[networkawareutil.CostKey]int64,
	networkTopology *ntv1alpha1.NetworkTopology,
	region string,
	zone string) {
	for _, w := range networkTopology.Spec.Weights { // Check the weights List
		if w.Name != no.weightsName { // If it is not the Preferred algorithm, continue
			continue
		}

		if region != "" { // Add Region Costs
			// Binary search through CostList: find the Topology Key for region
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyRegion)

			if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
				// Sort Costs by origin, might not be sorted since were manually defined
				sort.Sort(networkawareutil.ByOrigin(topologyList))
			}

			// Binary search through TopologyList: find the costs for the given Region
			costs := networkawareutil.FindOriginCosts(topologyList, region)

			// Add Region Costs
			for _, c := range costs {
				costMap[networkawareutil.CostKey{ // Add the cost to the map
					Origin:      region,
					Destination: c.Destination}] = c.NetworkCost
			}
		}
		if zone != "" { // Add Zone Costs
			// Binary search through CostList: find the Topology Key for zone
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyZone)

			if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
				// Sort Costs by origin, might not be sorted since were manually defined
				sort.Sort(networkawareutil.ByOrigin(topologyList))
			}

			// Binary search through TopologyList: find the costs for the given Region
			costs := networkawareutil.FindOriginCosts(topologyList, zone)

			// Add Zone Costs
			for _, c := range costs {
				costMap[networkawareutil.CostKey{ // Add the cost to the map
					Origin:      zone,
					Destination: c.Destination}] = c.NetworkCost
			}
		}
	}
}

// checkMaxNetworkCostRequirements : verifies the number of met and unmet dependencies based on the pod being filtered
func checkMaxNetworkCostRequirements(
	logger klog.Logger,
	scheduledList networkawareutil.ScheduledList,
	dependencyList []agv1alpha1.DependenciesInfo,
	nodeInfo *framework.NodeInfo,
	region string,
	zone string,
	costMap map[networkawareutil.CostKey]int64,
	no *NetworkOverhead) (int64, int64, error) {
	var satisfied int64 = 0
	var violated int64 = 0

	// check if maxNetworkCost fits
	for _, podAllocated := range scheduledList { // For each pod already allocated
		if podAllocated.Hostname != "" { // if hostname not empty...
			for _, d := range dependencyList { // For each pod dependency
				// If the pod allocated is not an established dependency, continue.
				if podAllocated.Selector != d.Workload.Selector {
					continue
				}

				// If the Pod hostname is the node being filtered, requirements are checked via extended resources
				if podAllocated.Hostname == nodeInfo.Node().Name {
					satisfied += 1
					continue
				}

				// If Nodes are not the same, get NodeInfo from pod Hostname
				podNodeInfo, err := no.handle.SnapshotSharedLister().NodeInfos().Get(podAllocated.Hostname)
				if err != nil {
					logger.Error(err, "getting pod's NodeInfo from snapshot", "nodeInfo", podNodeInfo)
					return satisfied, violated, err
				}

				// Get zone and region from Pod Hostname
				regionPodNodeInfo := networkawareutil.GetNodeRegion(podNodeInfo.Node())
				zonePodNodeInfo := networkawareutil.GetNodeZone(podNodeInfo.Node())

				if regionPodNodeInfo == "" && zonePodNodeInfo == "" { // Node has no zone and region defined
					violated += 1
				} else if region == regionPodNodeInfo { // If Nodes belong to the same region
					if zone == zonePodNodeInfo { // If Nodes belong to the same zone
						satisfied += 1
					} else { // belong to a different zone, check maxNetworkCost
						cost, costOK := costMap[networkawareutil.CostKey{ // Retrieve the cost from the map (origin: zone, destination: pod zoneHostname)
							Origin:      zone, // Time Complexity: O(1)
							Destination: zonePodNodeInfo,
						}]
						if costOK {
							if cost <= d.MaxNetworkCost {
								satisfied += 1
							} else {
								violated += 1
							}
						}
					}
				} else { // belong to a different region
					cost, costOK := costMap[networkawareutil.CostKey{ // Retrieve the cost from the map (origin: zone, destination: pod zoneHostname)
						Origin:      region, // Time Complexity: O(1)
						Destination: regionPodNodeInfo,
					}]
					if costOK {
						if cost <= d.MaxNetworkCost {
							satisfied += 1
						} else {
							violated += 1
						}
					}
				}
			}
		}
	}
	return satisfied, violated, nil
}

// getAccumulatedCost : calculate the accumulated cost based on the Pod's dependencies
func (no *NetworkOverhead) getAccumulatedCost(
	scheduledList networkawareutil.ScheduledList,
	dependencyList []agv1alpha1.DependenciesInfo,
	nodeName string,
	region string,
	zone string,
	costMap map[networkawareutil.CostKey]int64) (int64, error) {
	// keep track of the accumulated cost
	var cost int64 = 0

	// calculate accumulated shortest path
	for _, podAllocated := range scheduledList { // For each pod already allocated
		for _, d := range dependencyList { // For each pod dependency
			// If the pod allocated is not an established dependency, continue.
			if podAllocated.Selector != d.Workload.Selector {
				continue
			}

			if podAllocated.Hostname == nodeName { // If the Pod hostname is the node being scored
				cost += SameHostname
			} else { // If Nodes are not the same
				// Get NodeInfo from pod Hostname
				podNodeInfo, err := no.handle.SnapshotSharedLister().NodeInfos().Get(podAllocated.Hostname)
				if err != nil {
					no.logger.Error(err, "getting pod hostname from Snapshot", "nodeInfo", podNodeInfo)
					return cost, err
				}
				// Get zone and region from Pod Hostname
				regionPodNodeInfo := networkawareutil.GetNodeRegion(podNodeInfo.Node())
				zonePodNodeInfo := networkawareutil.GetNodeZone(podNodeInfo.Node())

				if regionPodNodeInfo == "" && zonePodNodeInfo == "" { // Node has no zone and region defined
					cost += MaxCost
				} else if region == regionPodNodeInfo { // If Nodes belong to the same region
					if zone == zonePodNodeInfo { // If Nodes belong to the same zone
						cost += SameZone
					} else { // belong to a different zone
						value, ok := costMap[networkawareutil.CostKey{ // Retrieve the cost from the map (origin: zone, destination: pod zoneHostname)
							Origin:      zone, // Time Complexity: O(1)
							Destination: zonePodNodeInfo,
						}]
						if ok {
							cost += value // Add the cost to the sum
						} else {
							cost += MaxCost
						}
					}
				} else { // belong to a different region
					value, ok := costMap[networkawareutil.CostKey{ // Retrieve the cost from the map (origin: region, destination: pod regionHostname)
						Origin:      region, // Time Complexity: O(1)
						Destination: regionPodNodeInfo,
					}]
					if ok {
						cost += value // Add the cost to the sum
					} else {
						cost += MaxCost
					}
				}
			}
		}
	}
	return cost, nil
}

func getPreFilterState(cycleState *framework.CycleState) (*PreFilterState, error) {
	no, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("error reading %q from cycleState: %w", preFilterStateKey, err)
	}

	state, ok := no.(*PreFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to NetworkOverhead.preFilterState error", no)
	}
	return state, nil
}

func (no *NetworkOverhead) findAppGroupNetworkOverhead(ctx context.Context, agName string) *agv1alpha1.AppGroup {
	no.logger.V(6).Info("Debugging namespaces", "namespaces", no.namespaces)
	for _, namespace := range no.namespaces {
		no.logger.V(6).Info("appGroup CR", "namespace", namespace, "name", agName)
		// AppGroup could not be placed in several namespaces simultaneously
		appGroup := &agv1alpha1.AppGroup{}
		err := no.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      agName,
		}, appGroup)
		if err != nil {
			no.logger.V(4).Error(err, "Cannot get AppGroup from AppGroupNamespaceLister:")
			continue
		}
		if appGroup != nil && appGroup.GetUID() != "" {
			return appGroup
		}
	}
	return nil
}

func (no *NetworkOverhead) findNetworkTopologyNetworkOverhead(ctx context.Context) *ntv1alpha1.NetworkTopology {
	no.logger.V(6).Info("Debugging namespaces", "namespaces", no.namespaces)
	for _, namespace := range no.namespaces {
		no.logger.V(6).Info("networkTopology CR:", "namespace", namespace, "name", no.ntName)
		// NetworkTopology could not be placed in several namespaces simultaneously
		networkTopology := &ntv1alpha1.NetworkTopology{}
		err := no.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      no.ntName,
		}, networkTopology)
		if err != nil {
			no.logger.V(4).Error(err, "Cannot get networkTopology from networkTopologyNamespaceLister:")
			continue
		}
		if networkTopology != nil && networkTopology.GetUID() != "" {
			return networkTopology
		}
	}
	return nil
}
