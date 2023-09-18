package resourcebasedzones

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gochache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	"sigs.k8s.io/scheduler-plugins/pkg/store"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

const Name = "ZoneResource"
const driverLabel = "habana.ai/hl_driver_version"

// ZoneResource is a pluging that schedule pods in a zone based on resource
type ZoneResource struct {
	ResourceNamespace string
	zoneLabel         string
	cleanupInterval   int
	frameworkHandler  framework.Handle
	pgm               pgclientset.Interface
	store             *store.InMemoryStore
	// backoffQueue hold pods belonging to a pod group that we want to move
	// to the back of the sorting queue, due to many failed attempts, and giving
	// chance to others
	backoffQueue *gochache.Cache
	// priorityZones is a list of scoring zones for single-card resources.
	// Order is important. First zone gets the highest score
	PriorityZones []string
}

var _ framework.PreFilterPlugin = &ZoneResource{}
var _ framework.FilterPlugin = &ZoneResource{}
var _ framework.ScorePlugin = &ZoneResource{}
var _ framework.PostBindPlugin = &ZoneResource{}

func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	args, ok := obj.(*config.ZoneResourceArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type ZoneResourceArgs, got %T", obj)
	}
	klog.V(4).Infof("zone resource: received args: %+v", args)
	klog.V(4).Info("Starting zone resource plugin")

	pgClient := pgclientset.NewForConfigOrDie(handle.KubeConfig())

	klog.V(4).Info("Started zone resource plugin")

	return &ZoneResource{
		cleanupInterval:   args.CleanupInterval,
		frameworkHandler:  handle,
		pgm:               pgClient,
		ResourceNamespace: args.ResourceNamespace,
		zoneLabel:         args.ZoneLabel,
		PriorityZones:     args.PriorityZones,
		store:             store.NewInMemoryStore(),
		backoffQueue:      gochache.New(3*time.Second, 15*time.Second),
	}, nil
}

func (zr *ZoneResource) Name() string {
	return Name
}

// Less is used to sort pods in the scheduling queue in the following order.
// 1. Compare the priorities of Pods.
// 2. Comprate the queues of the pod groups
// 3. Compaare the number of members in a group
// 4. Compare the initialization timestamps of PodGroups or Pods.
// 5. Compare the namespace+podname
func (zr *ZoneResource) Less(podInfo1, podInfo2 *framework.QueuedPodInfo) bool {
	klog.V(5).Infof(
		"Less(): comparing pod %s [%d] with %s [%d]", podInfo1.Pod.Name, podInfo1.Attempts,
		podInfo2.Pod.Name, podInfo2.Attempts,
	)

	// Get pod group
	pgName1 := util.GetPodGroupLabel(podInfo1.Pod)

	// if in backoff, return the other pod
	if pgName1 != "" {
		_, found := zr.backoffQueue.Get(pgName1)
		if found {
			// Pod in backoff is less important than the other
			klog.V(5).Infof("Less() %s in backoff", podInfo1.Pod.Name)
			return false
		}

		// if pod attempts % 10 == 0 - insert to backoff for 15s
		if podInfo1.Attempts%10 == 0 {
			klog.V(5).Infof("Less() %s attempts modolus ten", podInfo1.Pod.Name)
			zr.backoffQueue.Add(pgName1, nil, 15*time.Second)
			return false
		}
	}

	prio1 := corev1helpers.PodPriority(podInfo1.Pod)
	prio2 := corev1helpers.PodPriority(podInfo2.Pod)
	if prio1 != prio2 {
		return prio1 > prio2
	}

	queue1 := zr.scoreForQueue(podInfo1)
	queue2 := zr.scoreForQueue(podInfo2)

	if queue1 != queue2 {
		return queue1 > queue2
	}

	// Return pod with more members in group
	members1 := zr.minMembers(podInfo1.Pod)
	members2 := zr.minMembers(podInfo2.Pod)

	if members1 != members2 {
		return members1 > members2
	}

	creationTime1 := zr.GetCreationTimestamp(podInfo1.Pod, podInfo1.InitialAttemptTimestamp)
	creationTime2 := zr.GetCreationTimestamp(podInfo2.Pod, podInfo2.InitialAttemptTimestamp)
	if creationTime1.Equal(creationTime2) {
		return core.GetNamespacedName(podInfo1.Pod) < core.GetNamespacedName(podInfo2.Pod)
	}

	return creationTime1.Before(creationTime2)
}

func (zr *ZoneResource) GetCreationTimestamp(pod *corev1.Pod, ts time.Time) time.Time {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return ts
	}

	pg, err := zr.pgm.SchedulingV1alpha1().PodGroups(pod.Namespace).Get(context.TODO(), pgName, metav1.GetOptions{})
	if err != nil {
		return ts
	}
	return pg.CreationTimestamp.Time
}

func (zr *ZoneResource) minMembers(pod *corev1.Pod) int {
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return 1
	}

	pg, err := zr.pgm.SchedulingV1alpha1().PodGroups(pod.Namespace).Get(context.TODO(), pgName, metav1.GetOptions{})
	if err != nil {
		return 1
	}

	return int(pg.Spec.MinMember)
}

// TODO: remove as part of cleanup for not using queue label.
func (zr *ZoneResource) scoreForQueue(podInfo *framework.QueuedPodInfo) int {
	var val int
	switch podInfo.Pod.Annotations["habana.ai/queue"] {
	case "default":
		val = 1
	case "important":
		val = 10
	default:
		val = 0
	}

	return val
}

// PreFilter cleans the in-memory cache for a pod group that was added more than 5 minutes ago
// and still was not scheduled
func (zr *ZoneResource) PreFilter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod) *framework.Status {
	// Get Pod Group
	pgName := pod.Labels[v1alpha1.PodGroupLabel]

	// Check its creation time. If the interval passed, clean the cache
	if zr.shouldClean(pgName) {

		// Clean from group members any node they already have
		cleanedAll := true
		zr.frameworkHandler.IterateOverWaitingPods(func(wp framework.WaitingPod) {
			if wp.GetPod().Labels[v1alpha1.PodGroupLabel] == pgName {
				klog.V(4).Infof("cleaning selected node from pod %s [%s]", wp.GetPod().Name, pgName)
				if err := zr.frameworkHandler.ClientSet().CoreV1().Pods(wp.GetPod().Namespace).Bind(ctx, &corev1.Binding{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: wp.GetPod().Namespace,
						Name:      wp.GetPod().Name,
					},
					Target: corev1.ObjectReference{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       "",
					},
				}, metav1.CreateOptions{}); err != nil {
					klog.V(4).ErrorS(err, "Failed node cleanup on pod", "pod", wp.GetPod().Name)
					cleanedAll = false
				}
			}
		})
		if cleanedAll {
			klog.V(4).Infof("prefilter: Removed podgroup %s from cache", pgName)
			zr.store.Delete(pgName)
		}

	}

	return framework.NewStatus(framework.Success, "")
}

var (
	timeNow = time.Now
)

func (zr *ZoneResource) shouldClean(pgName string) bool {
	pgInfo, err := zr.store.Get(pgName)
	if err != nil {
		klog.V(5).Infof("shouldclean: pg not found in store: %s", pgName)
		return false
	}
	klog.V(5).Infof("shouldclean: pg %s added at: %s", pgName, pgInfo.Added.Format(time.RFC3339))
	return time.Since(pgInfo.Added).Round(time.Minute).Minutes() > float64(zr.cleanupInterval)
}

// PreFilterExtensions returns a PreFilterExtensions interface if the plugin implements one.
func (zr *ZoneResource) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func (zr *ZoneResource) Filter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	klog.V(5).InfoS("ZoneFilter", "pod", pod.Name)
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}
	// If pod is not habana resource, skip
	// TODO: add worker_node label to workers and check for it
	if _, ok := pod.Labels["habana.ai/user"]; !ok {
		klog.V(4).Info("non habana workload, skipping")
		return framework.NewStatus(framework.Success)
	}

	// If user provided his own zone selector, it's his responsability
	if zr.hasZoneAffinity(pod) {
		klog.V(4).InfoS("User provided zone selector", "pod", pod.Name)
		return framework.NewStatus(framework.Success)
	}

	klog.V(4).Infof("zone resource: pod: %s is trying to fit on node %s", pod.Name, nodeInfo.Node().Name)

	// ############################################################
	// Driver check
	podHLResource := zr.habanaResourceName(pod)
	if podHLResource == "" {
		klog.V(4).Info("no card resource request")
		return nil
	}
	reqResourceVal := reqHabanaResource(podHLResource, pod)

	// Block Driver version 1.10 on idc
	driverSemVer := pod.Annotations["habana.ai/release-build"]
	if strings.HasPrefix(driverSemVer, "1.10") && nodeInfo.Node().Labels["habana.ai/site"] == "idc" {
		klog.InfoS("driver version 1.10.X cannot run on IDC site", "pod", pod.Name, "node", nodeInfo.Node().Name)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "driver version 1.10.X cannot run on IDC site")
	}

	// TODO: look for 8 value
	// We filter out zones only if the user asked for it explicitly
	if pod.Annotations["habana.ai/strict_zone"] == "true" && reqResourceVal >= 8 {
		klog.V(4).Info("entered strict zone checking")

		pgName := pod.Labels[v1alpha1.PodGroupLabel]
		klog.V(4).Infof("pod %s belongs to pod group: %s", pod.Name, pgName)

		siteAffinity, _ := zr.hasSiteAffinity(pod)

		// Get zones and their free cards
		zones, err := zr.zones(nodeInfo, *pod.Spec.Priority, podHLResource, reqResourceVal, pod.Labels["habana.ai/schedulable"], siteAffinity)
		if err != nil {
			msg := fmt.Sprintf("failed calculating zones: %s", err.Error())
			return framework.NewStatus(framework.Error, msg)
		}

		// Log for verbosity
		for name, info := range zones {
			klog.V(4).Infof("Zone '%s' has %d free cards, possible preemption %d", name, info.freeCards, info.ToPreempt)
		}

		// Get total requested cards of the pod's group.
		totalCards, err := zr.totalGroupRequest(reqResourceVal, pgName)
		if err != nil {
			klog.Error(err)
			return framework.NewStatus(framework.Error, err.Error())
		}
		klog.V(4).Infof("Total request card for %s: %d", pgName, totalCards)

		var orederedZones []string
		if *pod.Spec.Priority > 0 {
			// Order of least preemptable
			orederedZones = zoneLessPreempt(zones)
		} else {
			// Order zone from the least free to most free by cards
			orederedZones = zoneFreeAsc(zones)
		}

		// Is PG Already scheduled

		// Check if the pod is member of a podgroup, is yes, get its members and check
		// if the a zone was already attached to one of the pod.
		var selectedZone string
		pgInfo, err := zr.store.Get(pgName)
		if err != nil {
			klog.ErrorS(err, "Retrieve pod group from store", "pod", pod.Name, "podgroup", pgName)
		}
		selectedZone = pgInfo.Zone
		klog.V(4).Infof("after group check for pod %s, selected zone is: %s", pod.Name, selectedZone)

		// if not, check if the node belongs to a free zone that can fit the members
		reqNodeZone := nodeInfo.Node().Labels[zr.zoneLabel]
		if selectedZone == "" {
			for _, zone := range orederedZones {
				klog.Infof("Pod group %s needs %d cards, zone %s has %d free cards", pgName, totalCards, zone, zones[zone])
				if zones[zone].freeCards >= totalCards {
					selectedZone = zone
					zr.store.Add(pgName, zone, timeNow())
				}
			}
		}

		zr.logPodGroup(pgName)

		// If we didn't find a zone at all
		if selectedZone == "" {
			return framework.NewStatus(framework.Unschedulable, "No zone can fulfill the request")
		}

		klog.V(4).Infof("Selected zone for pod %s is %s", pod.Name, selectedZone)
		state.Write(framework.StateKey(pgName), &pgData{
			zone: selectedZone,
		})
		// If the node is not in the selected zone, fail the request
		if reqNodeZone != selectedZone {
			klog.V(4).Infof("Not in the selected zone %s: %s [%s]", selectedZone, nodeInfo.Node().Name, reqNodeZone)
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Not in the selected zone %s", selectedZone))
		}
	}

	return nil
}

func (zr *ZoneResource) logPodGroup(pgName string) {
	zr.frameworkHandler.IterateOverWaitingPods(func(wp framework.WaitingPod) {
		if wp.GetPod().Labels[v1alpha1.PodGroupLabel] == pgName {
			klog.V(5).InfoS(
				"pod group details",
				"podgroup", pgName,
				"pod", wp.GetPod().Name,
				"node", wp.GetPod().Spec.NodeName,
				"nominated_node", wp.GetPod().Status.NominatedNodeName,
			)
		}
	})
}

func (zr *ZoneResource) Score(ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) (int64, *framework.Status) {

	// If pod is not habana resource, skip
	if _, ok := p.Labels["habana.ai/user"]; !ok {
		return 0, nil
	}

	nodeInfo, err := zr.frameworkHandler.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	pgName := util.GetPodGroupLabel(p)

	var zone string
	selectedZoneData, err := state.Read(framework.StateKey(pgName))
	if err == nil { // ERROR IS NIL
		var ok bool
		selectedZone, ok := selectedZoneData.(*pgData)
		if ok {
			zone = selectedZone.zone
		}
	}
	// If we have a podGroup name for the pod, and we found a selected zone,
	// we score only the nodes belonging to the zone.
	if pgName != "" && zone != "" {

		// If we have a selected zone, we want to give score only to nodes belonging to the zone,
		// otherwise we'll give zero.
		if nodeInfo.Node().Labels[zr.zoneLabel] == zone {
			klog.V(5).Infof("score: node %s belongs to selected zone %s. pgname: %s", nodeInfo.Node().Name, zone, pgName)
			return 100, nil
		} else {
			klog.V(5).Infof("score: node %s does not belong to selected zone %s. pgname: %s", nodeInfo.Node().Name, zone, pgName)
			return 0, nil
		}
	}

	nodeScore := zr.score(nodeInfo.Node().Labels[zr.zoneLabel])
	klog.V(5).Infof("node %s scored %d for pod %s", nodeName, nodeScore, p.Name)

	// nil is considered succcess
	return nodeScore, nil
}

func (zr *ZoneResource) hasZoneAffinity(pod *corev1.Pod) bool {
	if pod.Spec.Affinity != nil {
		if pod.Spec.Affinity.NodeAffinity != nil {
			required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if required != nil {
				for _, term := range required.NodeSelectorTerms {
					for _, expr := range term.MatchExpressions {
						if expr.Key == zr.zoneLabel && expr.Operator == corev1.NodeSelectorOpIn {
							klog.V(5).InfoS("Pod has zone affinity", "pod", pod.Name, "affinity", expr.String())
							return true
						}
					}
				}
			}
		}
	}

	return false
}

// hasSiteAffinity checks is user provided selector for habana.ai/site and returns its value.
func (zr *ZoneResource) hasSiteAffinity(pod *corev1.Pod) (string, bool) {
	if pod.Spec.Affinity != nil {
		if pod.Spec.Affinity.NodeAffinity != nil {
			required := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if required != nil {
				for _, term := range required.NodeSelectorTerms {
					for _, expr := range term.MatchExpressions {
						if expr.Key == "habana.ai/site" {
							klog.V(5).InfoS("Pod has site affinity", "pod", pod.Name, "affinity", expr.String())
							return expr.Values[0], true
						}
					}
				}
			}
		}
	}

	return "", false
}

func (zr *ZoneResource) score(zone string) int64 {
	score := 100
	var found bool
	for _, sz := range zr.PriorityZones {
		if sz == zone {
			found = true
			break
		}
		score -= 10
	}
	if !found {
		return 1
	}
	return int64(score)
}

func (zr *ZoneResource) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// PostBind is called after a pod is successfully bound. These plugins are used update PodGroup when pod is bound.
func (zr *ZoneResource) PostBind(ctx context.Context, _ *framework.CycleState, pod *corev1.Pod, nodeName string) {
	klog.V(5).Info("ZoneResource PostBind")
	// If pod group is in scheduled mode, we can clean it from the memory store
	pgName := pod.Labels[v1alpha1.PodGroupLabel]
	zr.store.Delete(pgName)
	klog.V(4).Infof("%s removed from store in PostBind", pgName)
}

// TODO
type zoneInfo struct {
	freeCards int64
	ToPreempt int
}

func (zr *ZoneResource) zones(nodeInfo *framework.NodeInfo, priority int32, resourceName corev1.ResourceName, requiredCards int, schedulable, siteAffinity string) (map[string]zoneInfo, error) {
	// zones is a map holding the zone (i.e a,b,c) and the number of the free cards
	zones := make(map[string]zoneInfo)

	nl, err := zr.frameworkHandler.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("zones list: %w", err)
	}
	for _, n := range nl {

		// Filter masters, services, non ready nodes out
		if isService(n.Node().Labels) || isMaster(n.Node().Labels) || !isReady(n.Node().Status.Conditions) {
			klog.V(5).Infof("node %s is not a worker node", n.Node().Name)
			continue
		}

		// If user asked for a specifiec site, we'll skip nodes not in the site
		if siteAffinity != "" && n.Node().Labels["habana.ai/site"] != siteAffinity {
			continue
		}

		klog.V(4).Infof("Node %s schedulable: %s | Pod schedulable value: %s", n.Node().Name, n.Node().Labels["habana.ai/schedulable"], schedulable)
		if n.Node().Labels["habana.ai/schedulable"] != schedulable {
			klog.V(5).Infof("schedulable value: %s, but node %s schedulable value is %s", schedulable, n.Node().Name, n.Node().Labels["habana.ai/schedulable"])
			continue
		}

		nodeZone, ok := n.Node().Labels[zr.zoneLabel]
		if !ok {
			klog.V(4).Infof("node %s does not have the zone label %s", n.Node().Name, zr.zoneLabel)
			continue
		}

		resFree, toPreempt := zr.freeResource(n, priority, resourceName)

		// Multi-HLS requested are expected to be with full-boxes, so we add only nodes
		// that can potentially hold 8-cards pods.
		// TODO: compare to pod request, than calculate if there are enough nodes for min member
		if resFree < int64(requiredCards) {
			klog.V(4).Infof("node %s [%d] cannot serve pod for %d cards", n.Node().Name, resFree, requiredCards)
			continue
		}

		klog.V(5).Infof("Resources free for node %s: %d", n.Node().Name, resFree)

		zoneVal := zones[nodeZone]
		zoneVal.freeCards += resFree
		zoneVal.ToPreempt += toPreempt

		zones[nodeZone] = zoneVal
	}

	return zones, nil
}

func isService(labels map[string]string) bool {
	if _, ok := labels["habana.ai/services"]; ok {
		return true
	}
	return false
}

func isMaster(labels map[string]string) bool {
	if _, ok := labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	return false
}

func isReady(conditions []corev1.NodeCondition) bool {
	for _, cond := range conditions {
		if cond.Status == corev1.ConditionTrue && cond.Type == corev1.NodeReady {
			return true
		}
	}
	return false
}

// freeResource calculcated the number of free cards on a node, based on the priority provided,
// taking into account the preemption ability, and returns the number of potential and actual free cards.
func (zr *ZoneResource) freeResource(node *framework.NodeInfo, priority int32, resourceName corev1.ResourceName) (int64, int) {
	allocatable := node.Node().Status.Allocatable[resourceName]
	klog.V(5).Infof("allocatable on node %s: %d", node.Node().Name, allocatable.Value())

	var habanaPods []*framework.PodInfo
	for _, p := range node.Pods {
		if _, ok := p.Pod.Labels["habana.ai/user"]; ok {
			habanaPods = append(habanaPods, p)
		}
	}

	// If it's a regular request, or it's a priority request but there are no workloads on
	// the node, calculate the free cards based on the allocatble minus in use on the node.
	inUse := node.Requested.ScalarResources[resourceName]
	klog.V(5).Infof("in use cards on node %s: %d", node.Node().Name, inUse)
	if priority == 0 || len(habanaPods) == 0 {
		return allocatable.Value() - inUse, 0
	}

	var potentialCards int64
	var toPreempt int
	for _, p := range habanaPods {
		if _, ok := p.Pod.Labels["habana.ai/user"]; !ok {
			continue
		}

		if priority > *p.Pod.Spec.Priority {
			podCards := p.Pod.Spec.Containers[0].Resources.Limits[resourceName]
			potentialCards += podCards.Value()
			toPreempt++
		}
	}

	// Check how much is in actual on the node, and add the number
	// of cards this pod can preempt.
	return (allocatable.Value() - inUse) + potentialCards, toPreempt
}

// habanaResourceName returns the requested value of Habana card of any type
func (zr *ZoneResource) habanaResourceName(pod *corev1.Pod) corev1.ResourceName {
	// TODO: check if String() method returns a full list of resource we can check

	for name := range pod.Spec.Containers[0].Resources.Limits {
		if strings.HasPrefix(string(name), zr.ResourceNamespace) {
			return name
		}
	}
	return ""
}

// reqHabanaResource returns the requested value of Habana card of any type
func reqHabanaResource(resName corev1.ResourceName, pod *corev1.Pod) int {
	resVal := pod.Spec.Containers[0].Resources.Limits[resName]

	return int(resVal.Value())
}

// zoneFreeAsc sort zone by the number of free cards in ascending order
func zoneFreeAsc(zones map[string]zoneInfo) []string {
	keys := make([]string, 0, len(zones))

	for k := range zones {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return zones[keys[i]].freeCards < zones[keys[j]].freeCards
	})

	return keys
}

func zoneLessPreempt(zones map[string]zoneInfo) []string {
	keys := make([]string, 0, len(zones))

	for k := range zones {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return (zones[keys[i]].ToPreempt < zones[keys[j]].ToPreempt)
	})

	return keys
}

// totalGroupRequest returns the total number of requested resource
// based on the number of pods, and not relaying on the pod group.
//
// This is due a problem with preemption when minResource value of PG
// exists.
func (zr *ZoneResource) totalGroupRequest(podReq int, pgName string) (int64, error) {
	// Make sure we get the correct value, otherwise pull value from PG.
	// Exclude launcher pod from required cards.
	pgReq, err := labels.NewRequirement(v1alpha1.PodGroupLabel, selection.Equals, []string{pgName})
	if err != nil {
		return 0, err
	}

	excludeLauncher, err := labels.NewRequirement("training.kubeflow.org/job-role", selection.NotEquals, []string{"launcher"})
	if err != nil {
		return 0, err
	}

	selector := labels.NewSelector().Add(*pgReq, *excludeLauncher)

	pods, err := zr.frameworkHandler.SharedInformerFactory().Core().V1().Pods().Lister().List(selector)
	if err != nil {
		return 0, err
	}

	members := len(pods)
	return int64(podReq * members), nil
}

type pgData struct {
	zone string
}

func (pd *pgData) Clone() framework.StateData {
	return pd
}

// alreadyScheduled returns a zone value if one of the members of a
// pod group or an empty string if none.
// func (zr *ZoneResource) alreadyScheduled(state *framework.CycleState, pgName string) (string, error) {
// 	val, err := state.Read(framework.StateKey(pgName))
// 	if err != nil {
// 		if errors.Is(err, framework.ErrNotFound) {
// 			klog.V(4).Info("key for zoneInfo not found")
// 			return "", nil
// 		}
// 		return "", err
// 	}

// 	data, ok := val.(*pgData)
// 	if !ok {
// 		klog.V(4).ErrorS(errors.New("failed type converstion"), "alreadySchedued")
// 		return "", err
// 	}

// 	if data.zone != "" {
// 		klog.V(4).Infof("zone from cycle is not empty: %s", data.zone)
// 		return data.zone, nil
// 	}

// 	pods, err := zr.frameworkHandler.SharedInformerFactory().Core().V1().Pods().Lister().List(
// 		labels.SelectorFromSet(labels.Set{v1alpha1.PodGroupLabel: pgName}),
// 	)
// 	if err != nil {
// 		return "", err
// 	}

// 	for _, pod := range pods {
// 		if pod.Spec.NodeName != "" {
// 			nodeInfo, err := zr.frameworkHandler.SnapshotSharedLister().NodeInfos().Get(pod.Spec.NodeName)
// 			if err != nil {
// 				return "", err
// 			}
// 			zone := nodeInfo.Node().Labels[zr.zoneLabel]
// 			state.Write(framework.StateKey(pgName), &pgData{
// 				zone: zone,
// 			})
// 			klog.V(4).Infof("Found one of the members already bounded to a zone: %s", zone)
// 			return zone, nil
// 		}
// 	}

// 	return "", nil
// }
