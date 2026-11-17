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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"
	"sigs.k8s.io/scheduler-plugins/pkg/util"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
)

var _ fwk.PreFilterPlugin = &NetworkOverhead{}
var _ fwk.FilterPlugin = &NetworkOverhead{}
var _ fwk.ScorePlugin = &NetworkOverhead{}
var _ fwk.EnqueueExtensions = &NetworkOverhead{}

const (
	// Name : name of plugin used in the plugin registry and configurations.
	Name = "NetworkOverhead"

	// MaxCost : MaxCost used in the NetworkTopology for costs between origins and destinations
	MaxCost = 10000

	// SameHostname : If pods belong to the same host, then consider cost as 0
	SameHostname = 0

	// SameZone : If pods belong to hosts in the same zone, then consider cost as 1
	SameZone = 1

	// preFilterStateKey is the key in CycleState to NetworkOverhead pre-computed data.
	preFilterStateKey = "PreFilter" + Name

	// Inter : Inter statistics key.
	Inter = "Inter"

	// Intra : Intra statistics key.
	Intra = "Intra"
)

var scheme = runtime.NewScheme()

// zeroQuantity is a package-level zero resource.Quantity used by statsFound to avoid
// allocating a new Quantity on every call.
var zeroQuantity = resource.MustParse("0")

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
	handle      fwk.Handle
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

	// AppGroupStatistics AvgBandwidth
	appGroupStatisticsAvgBandwidth resource.Quantity

	// AppGroupStatistics AvgCost
	appGroupStatisticsAvgCost int64

	// WorkloadStatistics
	workloadStatistics agv1alpha1.AppGroupStatistics

	// statisticsMap for inter- and intra- statistics
	statisticsMap map[networkawareutil.StatisticsKey]ntv1alpha1.NetworkTopologyStatistics
}

// Clone the preFilter state.
func (no *PreFilterState) Clone() fwk.StateData {
	return no
}

// Name : returns name of the plugin.
func (no *NetworkOverhead) Name() string {
	return Name
}

// EventsToRegister declares which cluster events should wake a pod this plugin
// has rejected back out of the scheduler's Unschedulable queue.
//
// Without this, NetworkOverhead falls back to the framework's default
// EnqueueExtensions, which only covers a fixed set of built-in Kubernetes
// resource kinds — it can never include AppGroup/NetworkTopology CRDs. A pod
// rejected here because its AppGroup or NetworkTopology CR wasn't found yet
// (a transient, usually sub-second informer-cache race right after the CR is
// created) would then never be requeued once that CR actually appears — it
// sits in the Unschedulable queue until the scheduler's unconditional ~5
// minute safety-net sweep instead of being retried immediately.
//
// The QueueingHintFns prevent two compounding problems:
//
//  1. O(N²) churn: without hints every AppGroup or NetworkTopology Add/Update
//     event sweeps every unschedulable pod. With N pods and ~N AppGroup
//     status-patches per batch (one per pod bind from appgroup-controller),
//     that is O(N²) evaluations on the scheduler's single-threaded main loop.
//
//  2. Total silence at large N: when pod-binding pauses, the AppGroup patch
//     stream goes quiet. Without a correct hint the scheduler waits for the
//     5-minute safety-net (podMaxInUnschedulablePodsDuration), which races
//     and often loses to test-harness WaitForRunningPods timeouts.
func (no *NetworkOverhead) EventsToRegister(_ context.Context) ([]fwk.ClusterEventWithHint, error) {
	agGVK := fmt.Sprintf("appgroups.%v.%v",
		agv1alpha1.SchemeGroupVersion.Version, agv1alpha1.SchemeGroupVersion.Group)
	ntGVK := fmt.Sprintf("networktopologies.%v.%v",
		ntv1alpha1.SchemeGroupVersion.Version, ntv1alpha1.SchemeGroupVersion.Group)
	return []fwk.ClusterEventWithHint{
		{Event: fwk.ClusterEvent{Resource: fwk.Pod, ActionType: fwk.Add}},
		{
			Event:          fwk.ClusterEvent{Resource: fwk.EventResource(agGVK), ActionType: fwk.Add | fwk.Update},
			QueueingHintFn: no.isAppGroupRelevantForPod,
		},
		{
			Event:          fwk.ClusterEvent{Resource: fwk.EventResource(ntGVK), ActionType: fwk.Add | fwk.Update},
			QueueingHintFn: no.isNetworkTopologyRelevantForPod,
		},
	}, nil
}

// isAppGroupRelevantForPod is the QueueingHintFn for AppGroup Add/Update events.
// It returns Queue only when the updated AppGroup's name matches the
// AppGroup label on the pod (i.e. this pod belongs to that AppGroup), and
// QueueSkip otherwise.
//
// Using the name alone (not name+namespace) is intentional: AppGroup names are
// unique within the cluster in practice (one AG per workload batch), so a name
// match is sufficient to identify the relevant update without knowing the
// namespace upfront. This cannot produce a false QueueSkip — at worst a name
// collision from a different namespace would produce a spurious Queue,
// which degrades gracefully to today's default behaviour.
func (no *NetworkOverhead) isAppGroupRelevantForPod(logger klog.Logger, pod *corev1.Pod, oldObj, newObj interface{}) (fwk.QueueingHint, error) {
	ag, ok := newObj.(*agv1alpha1.AppGroup)
	if !ok {
		logger.V(5).Info("NetworkOverhead hint: unexpected newObj type for AppGroup event, allowing retry",
			"pod", klog.KObj(pod), "type", fmt.Sprintf("%T", newObj))
		return fwk.Queue, nil
	}
	podAGName := networkawareutil.GetPodAppGroupLabel(pod)
	if podAGName == "" {
		// Pod has no AppGroup label — it was not rejected for AG reasons.
		return fwk.QueueSkip, nil
	}
	if ag.Name != podAGName {
		return fwk.QueueSkip, nil
	}
	return fwk.Queue, nil
}

// isNetworkTopologyRelevantForPod is the QueueingHintFn for NetworkTopology
// Add/Update events. It returns Queue only when the updated
// NetworkTopology's name matches the configured ntName (the one this plugin
// reads), and QueueSkip otherwise.
func (no *NetworkOverhead) isNetworkTopologyRelevantForPod(logger klog.Logger, pod *corev1.Pod, oldObj, newObj interface{}) (fwk.QueueingHint, error) {
	nt, ok := newObj.(*ntv1alpha1.NetworkTopology)
	if !ok {
		logger.V(5).Info("NetworkOverhead hint: unexpected newObj type for NetworkTopology event, allowing retry",
			"pod", klog.KObj(pod), "type", fmt.Sprintf("%T", newObj))
		return fwk.Queue, nil
	}
	if nt.Name != no.ntName {
		return fwk.QueueSkip, nil
	}
	return fwk.Queue, nil
}

func getArgs(obj runtime.Object) (*pluginconfig.NetworkOverheadArgs, error) {
	NetworkOverheadArgs, ok := obj.(*pluginconfig.NetworkOverheadArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NetworkOverhead, got %T", obj)
	}

	return NetworkOverheadArgs, nil
}

// ScoreExtensions : an interface for Score extended functionality
func (no *NetworkOverhead) ScoreExtensions() fwk.ScoreExtensions {
	return no
}

// New : create an instance of a NetworkOverhead plugin
func New(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)
	logger.V(4).Info("Creating new instance of the NetworkOverhead plugin")

	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	// The client reads AppGroup and NetworkTopology CRs from a local informer cache,
	// so PreFilter does not issue an API-server round-trip on every scheduling cycle.
	c, _, err := util.NewClientWithCachedReader(ctx, handle.KubeConfig(), scheme)
	if err != nil {
		return nil, err
	}

	no := &NetworkOverhead{
		Client:      c,
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
// 7. Get AppGroup and Workload Statistics to be used in filtering and scoring
func (no *NetworkOverhead) PreFilter(ctx context.Context, state fwk.CycleState, pod *corev1.Pod, nodes []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
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
		return nil, fwk.NewStatus(fwk.Success, "Pod does not belong to an AppGroup, return")
	}

	// Get AppGroup CR: try the pod's own namespace first, which avoids scanning all
	// configured namespaces and logging spurious errors on every first attempt.
	appGroup := no.findAppGroupNetworkOverhead(ctx, agName, pod.Namespace)
	if appGroup == nil {
		logger.V(4).Info("AppGroup CR not found, scoring all nodes equally", "appGroup", agName)
		return nil, fwk.NewStatus(fwk.Success, "AppGroup CR not found, scoring equally")
	}

	// Get NetworkTopology CR: try the pod's own namespace first for the same reason.
	networkTopology := no.findNetworkTopologyNetworkOverhead(ctx, pod.Namespace)
	if networkTopology == nil {
		logger.V(4).Info("NetworkTopology CR not found, scoring all nodes equally", "name", no.ntName)
		return nil, fwk.NewStatus(fwk.Success, "NetworkTopology CR not found, scoring equally")
	}

	// Warn if the NT controller has never completed a calculation (async lag after initial deploy).
	if networkTopology.Status.WeightCalculationTime.IsZero() {
		logger.V(4).Info("NetworkTopology WeightCalculationTime is zero: NT controller may not have run yet; topology costs may be incomplete", "name", no.ntName)
	}

	// Sort Costs if manual weights were selected
	no.sortNetworkTopologyCosts(networkTopology)

	// Get Dependencies of the given pod
	dependencyList := networkawareutil.GetDependencyList(pod, appGroup)
	score := false

	// If the pod has no dependencies, return
	if dependencyList == nil {
		score = true
		//	return nil, fwk.NewStatus(fwk.Success, "Pod has no dependencies, return")
	}

	// Get pods from lister
	selector := labels.Set(map[string]string{agv1alpha1.AppGroupLabel: agName}).AsSelector()
	pods, err := no.podLister.List(selector)
	if err != nil {
		return nil, fwk.NewStatus(fwk.Success, "Error while returning pods from appGroup, return")
	}

	// Return if pods are not yet allocated for the AppGroup...
	if len(pods) == 0 {
		score = true
		//	return nil, fwk.NewStatus(fwk.Success, "No pods yet allocated, return")
	}

	// Pods already scheduled: Get Scheduled List (Deployment name, replicaID, hostname)
	var scheduledList networkawareutil.ScheduledList
	if len(pods) != 0 {
		scheduledList = networkawareutil.GetScheduledList(pods)
	}

	// Check if scheduledList is empty...
	if len(scheduledList) == 0 {
		score = true
		//	logger.Error(nil, "Scheduled list is empty, return")
		//	return nil, fwk.NewStatus(fwk.Success, "Scheduled list is empty, return")
	}

	// Get all nodes
	nodeList, err := no.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fwk.NewStatus(fwk.Error, fmt.Sprintf("Error getting the nodelist: %v", err))
	}

	// Create variables to fill PreFilterState
	nodeCostMap := make(map[string]map[networkawareutil.CostKey]int64)
	satisfiedMap := make(map[string]int64)
	violatedMap := make(map[string]int64)
	finalCostMap := make(map[string]int64)
	statisticsMap := make(map[networkawareutil.StatisticsKey]ntv1alpha1.NetworkTopologyStatistics)

	// For each node:
	// 1 - Get region and zone labels
	// 2 - Calculate satisfied and violated number of dependencies
	// 3 - Calculate the final cost of the node to be used by the scoring plugin
	for _, nodeInfo := range nodeList {
		// retrieve region, zone and segment labels
		region := networkawareutil.GetNodeRegion(nodeInfo.Node())
		zone := networkawareutil.GetNodeZone(nodeInfo.Node())
		segment := networkawareutil.GetNodeSegment(nodeInfo.Node())
		logger.V(6).Info("Node info",
			"name", nodeInfo.Node().Name,
			"region", region,
			"zone", zone,
			"segment", segment)

		// Populate statistics map for the given node (always needed by Filter).
		no.populateStatisticsMap(logger, statisticsMap, networkTopology, region, zone, segment)
		// logger.V(6).Info("Map", "statisticsMap", statisticsMap)

		// costMap is only needed when there are scheduled pods to compare against.
		// Skip building it (and nodeCostMap) when score==true to avoid unnecessary work.
		if score {
			continue
		}

		// Create map for cost / destinations. Search for requirements faster...
		costMap := make(map[networkawareutil.CostKey]int64)

		// Populate cost map for the given node
		no.populateCostMap(costMap, networkTopology, region, zone, segment)
		// logger.V(6).Info("Map", "costMap", costMap)

		// Update nodeCostMap
		nodeCostMap[nodeInfo.Node().Name] = costMap

		// Get Satisfied and Violated number of dependencies
		if len(scheduledList) != 0 && dependencyList != nil {
			satisfied, violated, ok := checkMaxNetworkCostRequirements(logger, scheduledList, dependencyList, nodeInfo, region, zone, costMap, no)
			if ok != nil {
				return nil, fwk.NewStatus(fwk.Error, fmt.Sprintf("pod hostname not found: %v", ok))
			}

			// Update Satisfied and Violated maps
			satisfiedMap[nodeInfo.Node().Name] = satisfied
			violatedMap[nodeInfo.Node().Name] = violated
			logger.V(6).Info("Number of dependencies", "satisfied", satisfied, "violated", violated)

			// Get accumulated cost based on pod dependencies
			cost, ok := no.getAccumulatedCost(scheduledList, dependencyList, nodeInfo.Node().Name, region, zone, costMap)
			if ok != nil {
				return nil, fwk.NewStatus(fwk.Error, fmt.Sprintf("getting pod hostname from Snapshot: %v", ok))
			}
			logger.V(6).Info("Node final cost", "cost", cost)
			finalCostMap[nodeInfo.Node().Name] = cost
		}
	}

	appGroupStatisticsAvgBandwidth := appGroup.Status.AppGroupStatistics.AvgBandwidth
	appGroupStatisticsAvgCost := appGroup.Status.AppGroupStatistics.AvgCost
	// workloadStatisticsAvgCost := appGroup.Status.

	// Binary search to find Workload statistics of the given Workload
	workloadStatistics := networkawareutil.FindWorkloadStatistics(appGroup.Status.TopologyOrder, networkawareutil.GetPodAppGroupSelector(pod))

	// Update PreFilter State
	preFilterState = &PreFilterState{
		scoreEqually:                   score,
		agName:                         agName,
		appGroup:                       appGroup,
		networkTopology:                networkTopology,
		dependencyList:                 dependencyList,
		scheduledList:                  scheduledList,
		nodeCostMap:                    nodeCostMap,
		satisfiedMap:                   satisfiedMap,
		violatedMap:                    violatedMap,
		finalCostMap:                   finalCostMap,
		appGroupStatisticsAvgBandwidth: appGroupStatisticsAvgBandwidth,
		appGroupStatisticsAvgCost:      appGroupStatisticsAvgCost,
		workloadStatistics:             workloadStatistics,
		statisticsMap:                  statisticsMap,
	}

	state.Write(preFilterStateKey, preFilterState)
	return nil, fwk.NewStatus(fwk.Success, "PreFilter State updated")
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (no *NetworkOverhead) PreFilterExtensions() fwk.PreFilterExtensions {
	return no
}

// AddPod from pre-computed data in cycleState.
// no current need for the NetworkOverhead plugin
func (no *NetworkOverhead) AddPod(ctx context.Context,
	cycleState fwk.CycleState,
	podToSchedule *corev1.Pod,
	podToAdd fwk.PodInfo,
	nodeInfo fwk.NodeInfo) *fwk.Status {
	return fwk.NewStatus(fwk.Success, "")
}

// RemovePod from pre-computed data in cycleState.
// no current need for the NetworkOverhead plugin
func (no *NetworkOverhead) RemovePod(ctx context.Context,
	cycleState fwk.CycleState,
	podToSchedule *corev1.Pod,
	podToRemove fwk.PodInfo,
	nodeInfo fwk.NodeInfo) *fwk.Status {
	return fwk.NewStatus(fwk.Success, "")
}

// Filter : evaluate if node can respect maxNetworkCost requirements
func (no *NetworkOverhead) Filter(ctx context.Context,
	cycleState fwk.CycleState,
	pod *corev1.Pod,
	nodeInfo fwk.NodeInfo) *fwk.Status {
	if nodeInfo.Node() == nil {
		return fwk.NewStatus(fwk.Error, "node not found")
	}
	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "Filter")

	// Get PreFilterState
	preFilterState, err := getPreFilterState(cycleState)
	if err != nil {
		logger.Error(err, "Failed to read preFilterState from cycleState", "preFilterStateKey", preFilterStateKey)
		return fwk.NewStatus(fwk.Error, "not eligible due to failed to read from cycleState")
	}

	// If scoreEqually, return nil
	//if preFilterState.scoreEqually {
	//	logger.V(6).Info("Score all nodes equally, return")
	//	return nil
	//}

	// Check if scheduledList is empty...
	//if len(preFilterState.scheduledList) == 0 {
	//	logger.V(6).Info("Pod does not have dependencies based on AppGroup, scheduled list is empty, return")
	//	return nil
	//}

	// Get satisfied and violated number of dependencies
	if len(preFilterState.scheduledList) != 0 {
		logger.V(6).Info("Checking the number of violated dependencies... ")
		satisfied := preFilterState.satisfiedMap[nodeInfo.Node().Name]
		violated := preFilterState.violatedMap[nodeInfo.Node().Name]
		logger.V(6).Info("Number of dependencies:", "satisfied", satisfied, "violated", violated)

		// The pod is filtered out if the number of violated dependencies is higher than the satisfied ones
		if violated > satisfied {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("Node %v does not meet several network requirements from Workload dependencies: Satisfied: %v Violated: %v", nodeInfo.Node().Name, satisfied, violated))
		}
	}

	// Check AppGroup Statistics
	avgBandwidth := preFilterState.appGroupStatisticsAvgBandwidth
	avgCost := preFilterState.appGroupStatisticsAvgCost
	zone := networkawareutil.GetNodeZone(nodeInfo.Node())
	segment := networkawareutil.GetNodeSegment(nodeInfo.Node())

	intraZoneStatistics, okZone := preFilterState.statisticsMap[networkawareutil.StatisticsKey{
		Origin:         zone,
		TypeStatistics: Intra,
	}]

	intraSegmentStatistics, okSegment := preFilterState.statisticsMap[networkawareutil.StatisticsKey{
		Origin:         segment,
		TypeStatistics: Intra,
	}]

	workloadStatistics := preFilterState.workloadStatistics

	logger.V(6).Info("Checking Appgroup and Workload Statistics... ")
	logger.V(6).Info("AppGroup requirements:", "avgBandwidth", avgBandwidth, "avgCost", avgCost)
	logger.V(6).Info("Workload requirements:", "avgBandwidth", workloadStatistics.AvgBandwidth, "avgCost", workloadStatistics.AvgCost)

	logger.V(6).Info("Zone IntraStatistics:", "zone", zone, "minBandwidth", intraZoneStatistics.MinBandwidth, "avgCost", intraZoneStatistics.AvgCost)
	logger.V(6).Info("Segment IntraStatistics:", "segment", segment, "minBandwidth", intraSegmentStatistics.MinBandwidth, "avgCost", intraSegmentStatistics.AvgCost)

	// Zone Filtering functions
	// Only apply zone-based filtering if the segment label exists and statistics are available
	if zone != "" && okZone {
		// The pod is filtered out if AppGroup min bandwidth is higher than min bandwidth of zone
		if avgBandwidth.Cmp(intraZoneStatistics.MinBandwidth) == 1 {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v zone %v does not meet Appgroup bandwidth requirements. Zone: %v workload: %v.", nodeInfo.Node().Name, zone, intraZoneStatistics.MinBandwidth, avgBandwidth))
		}

		// The pod is filtered out if AppGroup avg cost is lower than avgCost of zone.
		// A zero cost means the statistic is not available (yet), so the check is skipped.
		if avgCost != 0 && intraZoneStatistics.AvgCost != 0 && avgCost <= intraZoneStatistics.AvgCost {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v zone %v does not meet Appgroup cost requirements.", nodeInfo.Node().Name, zone))
		}

		// The pod is filtered out if Workload avg bandwidth is higher than min bandwidth of zone
		if workloadStatistics.AvgBandwidth.Cmp(intraZoneStatistics.MinBandwidth) == 1 {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v zone %v does not meet Workload bandwidth requirements.", nodeInfo.Node().Name, zone))
		}

		// The pod is filtered out if AppGroup avg cost is lower than avgCost of zone
		if intraZoneStatistics.AvgCost != 0 && workloadStatistics.AvgCost != 0 {
			if workloadStatistics.AvgCost <= intraZoneStatistics.AvgCost {
				return fwk.NewStatus(fwk.Unschedulable,
					fmt.Sprintf("IntraStatistics - Node %v zone %v does not meet Workload cost requirements. Zone: %v workload: %v.", nodeInfo.Node().Name, zone, intraZoneStatistics.AvgCost, workloadStatistics.AvgCost))
			}
		}
	}

	// Segment Filtering functions
	// Only apply segment-based filtering if the segment label exists and statistics are available
	if segment != "" && okSegment {
		// The pod is filtered out if AppGroup min bandwidth is higher than min bandwidth of segment
		if avgBandwidth.Cmp(intraSegmentStatistics.MinBandwidth) == 1 {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v segment %v does not meet Appgroup bandwidth requirements. Segment: %v workload: %v.", nodeInfo.Node().Name, segment, intraSegmentStatistics.MinBandwidth, avgBandwidth))
		}

		// The pod is filtered out if AppGroup avg cost is lower than avgCost of segment.
		// A zero cost means the statistic is not available (yet), so the check is skipped.
		if avgCost != 0 && intraSegmentStatistics.AvgCost != 0 && avgCost <= intraSegmentStatistics.AvgCost {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v segment %v does not meet Appgroup cost requirements.", nodeInfo.Node().Name, segment))
		}

		// The pod is filtered out if Workload avg bandwidth is higher than min bandwidth of zone
		if workloadStatistics.AvgBandwidth.Cmp(intraSegmentStatistics.MinBandwidth) == 1 {
			return fwk.NewStatus(fwk.Unschedulable,
				fmt.Sprintf("IntraStatistics - Node %v segment %v does not meet Workload bandwidth requirements.", nodeInfo.Node().Name, segment))
		}

		// The pod is filtered out if AppGroup avg cost is lower than avgCost of segment
		if intraSegmentStatistics.AvgCost != 0 && workloadStatistics.AvgCost != 0 {
			if workloadStatistics.AvgCost <= intraSegmentStatistics.AvgCost {
				return fwk.NewStatus(fwk.Unschedulable,
					fmt.Sprintf("IntraStatistics - Node %v segment %v does not meet Workload cost requirements. Segment: %v workload: %v.", nodeInfo.Node().Name, segment, intraSegmentStatistics.AvgCost, workloadStatistics.AvgCost))
			}
		}
	}

	return nil
}

// Score : evaluate score for a node
func (no *NetworkOverhead) Score(ctx context.Context,
	cycleState fwk.CycleState,
	pod *corev1.Pod,
	nodeInfo fwk.NodeInfo) (int64, *fwk.Status) {
	score := fwk.MinNodeScore
	nodeName := nodeInfo.Node().Name

	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "Score")
	// Get PreFilterState
	preFilterState, err := getPreFilterState(cycleState)
	if err != nil {
		logger.Error(err, "Failed to read preFilterState from cycleState", "preFilterStateKey", preFilterStateKey)
		return score, fwk.NewStatus(fwk.Error, "not eligible due to failed to read from cycleState, return min score")
	}

	// If scoreEqually, return minScore
	if preFilterState.scoreEqually {
		return score, fwk.NewStatus(fwk.Success, "scoreEqually enabled: minimum score")
	}

	// Return Accumulated Cost as score
	if nodeScore, ok := preFilterState.finalCostMap[nodeName]; ok {
		score = nodeScore
	} else {
		logger.V(4).Info("Node not found in finalCostMap, returning max score before normalization", "node", nodeName)
		// Penalize the node since topology labels might be missing
		score = fwk.MaxNodeScore
	}

	logger.V(4).Info("Score:", "pod", pod.GetName(), "node", nodeName, "finalScore", score)
	return score, fwk.NewStatus(fwk.Success, "Accumulated cost added as score, normalization ensures lower costs are favored")
}

// NormalizeScore : normalize scores since lower scores correspond to lower latency
func (no *NetworkOverhead) NormalizeScore(ctx context.Context,
	state fwk.CycleState,
	pod *corev1.Pod,
	scores fwk.NodeScoreList) *fwk.Status {
	logger := klog.FromContext(klog.NewContext(ctx, no.logger)).WithValues("ExtensionPoint", "NormalizeScore")
	logger.V(4).Info("before normalization: ", "scores", scores)

	// Get Min and Max Scores to normalize between fwk.MaxNodeScore and fwk.MinNodeScore
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
			normCost = float64(fwk.MaxNodeScore) * float64(scores[i].Score-minCost) / float64(maxCost-minCost)
			scores[i].Score = fwk.MaxNodeScore - int64(normCost)
		} else { // If maxCost = minCost, avoid division by 0
			normCost = float64(scores[i].Score - minCost)
			scores[i].Score = fwk.MaxNodeScore - int64(normCost)
		}
	}
	logger.V(4).Info("after normalization: ", "scores", scores)
	return nil
}

// MinMax : get min and max scores from NodeScoreList
func getMinMaxScores(scores fwk.NodeScoreList) (int64, int64) {
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
	zone string,
	segment string) {
	for _, w := range networkTopology.Spec.Weights { // Check the weights List
		if w.Name != no.weightsName { // If it is not the Preferred algorithm, continue
			continue
		}

		if region != "" { // Add Region Costs
			// Binary search through CostList: find the Topology Key for region
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyRegion)

			if len(topologyList) > 0 {
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
		}
		if zone != "" { // Add Zone Costs
			// Binary search through CostList: find the Topology Key for zone
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyZone)

			if len(topologyList) > 0 {
				if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
					// Sort Costs by origin, might not be sorted since were manually defined
					sort.Sort(networkawareutil.ByOrigin(topologyList))
				}

				// Binary search through TopologyList: find the costs for the given Zone
				costs := networkawareutil.FindOriginCosts(topologyList, zone)

				// Add Zone Costs
				for _, c := range costs {
					costMap[networkawareutil.CostKey{ // Add the cost to the map
						Origin:      zone,
						Destination: c.Destination}] = c.NetworkCost
				}
			}
		}
		if segment != "" { // Add Segment Costs
			// Binary search through CostList: find the Topology Key for segment
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologySegment)

			if len(topologyList) > 0 {
				if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
					// Sort Costs by origin, might not be sorted since were manually defined
					sort.Sort(networkawareutil.ByOrigin(topologyList))
				}

				// Binary search through TopologyList: find the costs for the given Segment
				costs := networkawareutil.FindOriginCosts(topologyList, segment)

				// Add Segment Costs
				for _, c := range costs {
					costMap[networkawareutil.CostKey{ // Add the cost to the map
						Origin:      segment,
						Destination: c.Destination}] = c.NetworkCost
				}
			}
		}
	}
}

// populateStatisticsMap : Populates statisticsMap based on the node being filtered/scored
func (no *NetworkOverhead) populateStatisticsMap(
	logger klog.Logger,
	statisticsMap map[networkawareutil.StatisticsKey]ntv1alpha1.NetworkTopologyStatistics,
	networkTopology *ntv1alpha1.NetworkTopology,
	region string,
	zone string,
	segment string) {
	for _, w := range networkTopology.Spec.Weights { // Check the weights List
		if w.Name != no.weightsName { // If it is not the Preferred algorithm, continue
			continue
		}
		logger.V(6).Info("weights TopologyList: ", "list", w.TopologyList)
		if region != "" { // Add Region Statistics
			// Binary search through CostList: find the Topology Key for region
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyRegion)
			logger.V(6).Info("Region topologyList: ", "region", region, "list", topologyList)

			// Only populate statistics when the topology list is non-empty; writing zero-value
			// stats for a missing origin causes the filter to reject every node when
			// AppGroup.AvgBandwidth > 0.
			if len(topologyList) > 0 {
				if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
					// Sort Costs by origin, might not be sorted since were manually defined
					sort.Sort(networkawareutil.ByOrigin(topologyList))
				}

				// Binary search through TopologyList: find inter and intra statistics for the given Region
				intraStatistics := networkawareutil.FindOriginIntraStatistics(topologyList, region)
				interStatistics := networkawareutil.FindOriginInterStatistics(topologyList, region)

				logger.V(6).Info("Region intraStatistics: ", "list", intraStatistics)
				logger.V(6).Info("Region interStatistics: ", "list", interStatistics)

				// Only store non-zero statistics; a zero-value means the origin was not found.
				if statsFound(intraStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: region, TypeStatistics: Intra}] = intraStatistics
				}
				if statsFound(interStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: region, TypeStatistics: Inter}] = interStatistics
				}
			}
		}
		if zone != "" { // Add Zone Statistics
			// Binary search through CostList: find the Topology Key for zone
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologyZone)
			logger.V(6).Info("Zone topologyList: ", "zone", zone, "list", topologyList)

			if len(topologyList) > 0 {
				if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
					// Sort Costs by origin, might not be sorted since were manually defined
					sort.Sort(networkawareutil.ByOrigin(topologyList))
				}

				// Binary search through TopologyList: find inter and intra statistics for the given Zone
				intraStatistics := networkawareutil.FindOriginIntraStatistics(topologyList, zone)
				interStatistics := networkawareutil.FindOriginInterStatistics(topologyList, zone)

				logger.V(6).Info("Zone intraStatistics: ", "list", intraStatistics)
				logger.V(6).Info("Zone interStatistics: ", "list", interStatistics)

				if statsFound(intraStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: zone, TypeStatistics: Intra}] = intraStatistics
				}
				if statsFound(interStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: zone, TypeStatistics: Inter}] = interStatistics
				}
			}
		}
		if segment != "" { // Add Segment Statistics
			// Binary search through CostList: find the Topology Key for segment
			topologyList := networkawareutil.FindTopologyKey(w.TopologyList, ntv1alpha1.NetworkTopologySegment)
			logger.V(6).Info("Segment topologyList: ", "segment", segment, "list", topologyList)

			if len(topologyList) > 0 {
				if no.weightsName != ntv1alpha1.NetworkTopologyNetperfCosts {
					// Sort Costs by origin, might not be sorted since were manually defined
					sort.Sort(networkawareutil.ByOrigin(topologyList))
				}

				// Binary search through TopologyList: find inter and intra statistics for the given Segment
				intraStatistics := networkawareutil.FindOriginIntraStatistics(topologyList, segment)
				interStatistics := networkawareutil.FindOriginInterStatistics(topologyList, segment)

				logger.V(6).Info("Segment intraStatistics: ", "list", intraStatistics)
				logger.V(6).Info("Segment interStatistics: ", "list", interStatistics)

				if statsFound(intraStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: segment, TypeStatistics: Intra}] = intraStatistics
				}
				if statsFound(interStatistics) {
					statisticsMap[networkawareutil.StatisticsKey{Origin: segment, TypeStatistics: Inter}] = interStatistics
				}
			}
		}
	}
}

// statsFound returns true if s contains at least one non-zero field.
// FindOriginIntraStatistics / FindOriginInterStatistics return a zero-value struct when
// the origin is not found; storing that zero-value causes the bandwidth filter to reject
// every node when AppGroup.AvgBandwidth > 0.
func statsFound(s ntv1alpha1.NetworkTopologyStatistics) bool {
	return s.MinBandwidth.Cmp(zeroQuantity) != 0 ||
		s.AvgBandwidth.Cmp(zeroQuantity) != 0 ||
		s.MaxBandwidth.Cmp(zeroQuantity) != 0 ||
		s.MinCost != 0 || s.AvgCost != 0 || s.MaxCost != 0
}

// checkMaxNetworkCostRequirements : verifies the number of met and unmet dependencies based on the pod being filtered
func checkMaxNetworkCostRequirements(
	logger klog.Logger,
	scheduledList networkawareutil.ScheduledList,
	dependencyList []agv1alpha1.DependenciesInfo,
	nodeInfo fwk.NodeInfo,
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

func getPreFilterState(cycleState fwk.CycleState) (*PreFilterState, error) {
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

// findAppGroupNetworkOverhead looks up the AppGroup CR by name. It tries
// podNamespace first: the pod and its AppGroup are almost always in the same
// namespace, so this avoids iterating the full configured namespace list and
// logging a spurious error on every namespace miss on the first scheduling
// attempt. If not found there, it falls through to the rest of no.namespaces
// in order.
func (no *NetworkOverhead) findAppGroupNetworkOverhead(ctx context.Context, agName, podNamespace string) *agv1alpha1.AppGroup {
	namespacesToTry := namespacesWithPriorityFirst(podNamespace, no.namespaces)
	for _, namespace := range namespacesToTry {
		no.logger.V(6).Info("Looking up AppGroup", "namespace", namespace, "name", agName)
		appGroup := &agv1alpha1.AppGroup{}
		if err := no.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      agName,
		}, appGroup); err != nil {
			no.logger.V(6).Error(err, "AppGroup not found in namespace", "namespace", namespace)
			continue
		}
		if appGroup.GetUID() != "" {
			return appGroup
		}
	}
	no.logger.V(4).Info("AppGroup CR not found in any namespace", "name", agName)
	return nil
}

// findNetworkTopologyNetworkOverhead looks up the NetworkTopology CR by name.
// Like findAppGroupNetworkOverhead, it tries podNamespace first.
func (no *NetworkOverhead) findNetworkTopologyNetworkOverhead(ctx context.Context, podNamespace string) *ntv1alpha1.NetworkTopology {
	namespacesToTry := namespacesWithPriorityFirst(podNamespace, no.namespaces)
	for _, namespace := range namespacesToTry {
		no.logger.V(6).Info("Looking up NetworkTopology", "namespace", namespace, "name", no.ntName)
		networkTopology := &ntv1alpha1.NetworkTopology{}
		if err := no.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      no.ntName,
		}, networkTopology); err != nil {
			no.logger.V(6).Error(err, "NetworkTopology not found in namespace", "namespace", namespace)
			continue
		}
		if networkTopology.GetUID() != "" {
			return networkTopology
		}
	}
	no.logger.V(4).Info("NetworkTopology CR not found in any namespace", "name", no.ntName)
	return nil
}

// namespacesWithPriorityFirst returns a deduplicated namespace list with
// priorityNS at position 0, followed by the remaining entries in their
// original order. If priorityNS is empty or already the first entry, the
// original slice is returned as-is (no allocation).
func namespacesWithPriorityFirst(priorityNS string, namespaces []string) []string {
	if priorityNS == "" || (len(namespaces) > 0 && namespaces[0] == priorityNS) {
		return namespaces
	}
	result := make([]string, 0, len(namespaces)+1)
	result = append(result, priorityNS)
	for _, ns := range namespaces {
		if ns != priorityNS {
			result = append(result, ns)
		}
	}
	return result
}
