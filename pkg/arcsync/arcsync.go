package arcsync

import (
	"context"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name              = "ARCSync"
	RequiredNPUCount  = "ascend-ci.com/required-npu-count"
	ResourceDomain    = "ascend-ci.com/npu-resource-domain"
	ResourceModel     = "ascend-ci.com/npu-resource-model"
	AllocatedNPUCount = "ascend-ci.com/npu-count"
	stateKey          = Name + "/state"
)

type reservation struct {
	nodeName  string
	count     int64
	timestamp time.Time
	baseName  string
	namespace string
}

type ARCSync struct {
	handle               framework.Handle
	podLister            corev1listers.PodLister
	inFlightReservations map[string]reservation
	mu                   sync.Mutex
	nsOffloadingLister   nsOffloadingLister
	queueLister          queueLister
}

type preFilterState struct {
	requiredCount int64
	resourceName  v1.ResourceName
	nodeFreeNPU   map[string]int64
}

func (s *preFilterState) Clone() framework.StateData {
	return s
}

var _ framework.PreFilterPlugin = &ARCSync{}
var _ framework.FilterPlugin = &ARCSync{}
var _ framework.ScorePlugin = &ARCSync{}
var _ framework.ReservePlugin = &ARCSync{}
var _ framework.PostBindPlugin = &ARCSync{}
var _ framework.EnqueueExtensions = &ARCSync{}

func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	pl := &ARCSync{
		handle:               h,
		podLister:            h.SharedInformerFactory().Core().V1().Pods().Lister(),
		inFlightReservations: make(map[string]reservation),
	}

	dynamicClient, err := dynamic.NewForConfig(h.KubeConfig())
	if err != nil {
		return pl, nil
	}

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)

	var syncs []cache.InformerSynced

	nsOffloadingInformer := dynamicInformerFactory.ForResource(gvrNamespaceOffloading)
	if crdExists(dynamicClient, ctx, gvrNamespaceOffloading) {
		pl.nsOffloadingLister = &dynamicNSOffloadingLister{lister: nsOffloadingInformer.Lister()}
		go nsOffloadingInformer.Informer().Run(ctx.Done())
		syncs = append(syncs, nsOffloadingInformer.Informer().HasSynced)
	}

	queueInformer := dynamicInformerFactory.ForResource(gvrQueue)
	if crdExists(dynamicClient, ctx, gvrQueue) {
		pl.queueLister = &dynamicQueueLister{lister: queueInformer.Lister()}
		go queueInformer.Informer().Run(ctx.Done())
		syncs = append(syncs, queueInformer.Informer().HasSynced)
	}

	if len(syncs) > 0 {
		go func() {
			cache.WaitForCacheSync(ctx.Done(), syncs...)
		}()
	}

	return pl, nil
}

func crdExists(dc dynamic.Interface, ctx context.Context, gvr schema.GroupVersionResource) bool {
	_, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	return true
}

func (pl *ARCSync) Name() string {
	return Name
}

func (pl *ARCSync) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete | framework.Update | framework.Add}},
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add | framework.Update}},
	}, nil
}

// canScheduleOnNode excludes only cordoned nodes (Unschedulable: true).
// Tainted nodes are included — their NPU capacity is physically present and
// counts toward global admission decisions.
func canScheduleOnNode(node *v1.Node) bool {
	return !node.Spec.Unschedulable
}

func nodeMatchesSelector(node *v1.Node, selector map[string]string) bool {
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	return true
}

func getBaseName(name string) string {
	suffix := "-workflow"
	if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}

// isOldestPendingRunner returns true if no older unbound runner pod (same NPU type,
// same namespace, same scheduling pool) exists.
// This enforces strict FIFO within a scheduling pool: a runner pod only proceeds
// when it is the oldest waiting one in its pool. Using CreationTimestamp avoids the
// backoff side-effect where older (more-retried) pods accumulate longer backoff
// delays and get jumped by newer pods.
//
// Only unbound pods (Spec.NodeName == "") are compared — pods already assigned to
// a node are past the scheduling decision and must not block new pods. Namespace
// isolation prevents cross-namespace blocking. Pool-based grouping (via
// npuFIFOPool when NamespaceOffloading is active, or nodeSelector comparison
// otherwise) ensures that runners targeting different scheduling pools do not
// block each other.
func (pl *ARCSync) isOldestPendingRunner(pod *v1.Pod, nsHasOffloading bool) bool {
	if pl.podLister == nil {
		return true
	}
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	myTime := pod.CreationTimestamp.Time

	allPods, err := pl.podLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "ARCSync: failed to list pods for FIFO check, failing open")
		return true
	}

	myPool := npuFIFOPool(pod)

	for _, p := range allPods {
		if p.UID == pod.UID {
			continue
		}
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		if p.Spec.NodeName != "" {
			continue
		}
		if p.Namespace != pod.Namespace {
			continue
		}
		if p.Labels[RequiredNPUCount] == "" {
			continue
		}
		if p.Labels[ResourceDomain] != resDomain || p.Labels[ResourceModel] != resModel {
			continue
		}
		if nsHasOffloading {
			if npuFIFOPool(p) != myPool {
				continue
			}
		} else {
			hasUnsharedConstraint := false
			for k, v := range p.Spec.NodeSelector {
				if pod.Spec.NodeSelector[k] != v {
					hasUnsharedConstraint = true
					break
				}
			}
			if hasUnsharedConstraint {
				continue
			}
		}
		pTime := p.CreationTimestamp.Time
		if pTime.Before(myTime) || (pTime.Equal(myTime) && string(p.UID) < string(pod.UID)) {
			klog.V(4).InfoS("ARCSync: FIFO block — older runner exists",
				"pod", pod.Name, "olderPod", p.Name,
				"podCreated", myTime, "olderCreated", pTime)
			return false
		}
	}
	return true
}

func npuFIFOPool(pod *v1.Pod) string {
	if v, ok := pod.Spec.NodeSelector["liqo.io/remote-cluster-id"]; ok {
		return v
	}
	return "local"
}

func (pl *ARCSync) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	reqCountStr, ok := pod.Labels[RequiredNPUCount]
	if !ok {
		return nil, framework.NewStatus(framework.Success, "")
	}

	reqCount, _ := strconv.Atoi(reqCountStr)
	resDomain := pod.Labels[ResourceDomain]
	resModel := pod.Labels[ResourceModel]
	fullResourceName := v1.ResourceName(resDomain + "/" + resModel)

	nodeInfos, err := pl.handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, framework.NewStatus(framework.Error, "failed to get node snapshots: "+err.Error())
	}

	pl.mu.Lock()

	activeWorkflows := make(map[string]bool)
	knownPodUIDs := make(map[string]bool)
	nodePhysicalUsage := make(map[string]int64)
	nsLocalPhysicalUsage := make(map[string]int64)
	virtualNodes := make(map[string]bool)

	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}
		nodeName := node.Name
		var physUsage int64
		var nsUsage int64
		virt := isVirtualNode(node)
		if virt {
			virtualNodes[nodeName] = true
		}
		for _, podInfo := range nodeInfo.Pods {
			p := podInfo.Pod
			if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed || p.UID == pod.UID {
				continue
			}
			knownPodUIDs[string(p.UID)] = true
			baseName := getBaseName(p.Name)
			if p.Name != baseName {
				activeWorkflows[baseName] = true
			}
			if virt {
				continue
			}
			var podUsage int64
			for _, container := range p.Spec.Containers {
				if q, exists := container.Resources.Requests[fullResourceName]; exists {
					podUsage += q.Value()
				}
			}
			if val, exists := p.Labels[AllocatedNPUCount]; exists {
				if p.Labels[ResourceDomain] == resDomain && p.Labels[ResourceModel] == resModel {
					count, _ := strconv.ParseInt(val, 10, 64)
					if count > podUsage {
						podUsage = count
					}
				}
			}
			physUsage += podUsage
			if p.Namespace == pod.Namespace {
				nsUsage += podUsage
			}
		}
		if virt {
			physUsage = calcVirtualNodeOccupied(nodeInfo, resDomain, resModel)
		}
		nodePhysicalUsage[nodeName] = physUsage
		nsLocalPhysicalUsage[nodeName] = nsUsage
	}

	nodeTotalOccupied := make(map[string]int64)
	for nodeName, usage := range nodePhysicalUsage {
		nodeTotalOccupied[nodeName] = usage
	}

	now := time.Now()
	nsLocalReservated := make(map[string]int64)
	for uid, res := range pl.inFlightReservations {
		if !knownPodUIDs[uid] {
			if now.Sub(res.timestamp) > 10*time.Second {
				delete(pl.inFlightReservations, uid)
				continue
			}
		}
		if activeWorkflows[res.baseName] {
			continue
		}
		if virtualNodes[res.nodeName] {
			continue
		}
		nodeTotalOccupied[res.nodeName] += res.count
		if res.namespace == pod.Namespace {
			nsLocalReservated[res.nodeName] += res.count
		}
	}

	pl.mu.Unlock()

	nodeFreeNPU := make(map[string]int64)
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(node) {
			continue
		}
		if !nodeMatchesSelector(node, pod.Spec.NodeSelector) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		free := allocatable.Value() - nodeTotalOccupied[node.Name]
		nodeFreeNPU[node.Name] = free
	}

	var nsOffloading *unstructured.Unstructured
	nsHasOffloading := false
	if pl.nsOffloadingLister != nil {
		nsOffloading, nsHasOffloading, _ = pl.nsOffloadingLister.Get(pod.Namespace)
	}

	if nsHasOffloading && nsOffloading != nil {
		nsLocalOccupied := make(map[string]int64)
		for nodeName, usage := range nsLocalPhysicalUsage {
			nsLocalOccupied[nodeName] = usage + nsLocalReservated[nodeName]
		}
		pl.applyLiqoComparison(nodeInfos, pod, resDomain, resModel, fullResourceName, nsLocalOccupied, nodeFreeNPU, nsOffloading, int64(reqCount), virtualNodes)

		if queueLimit, qFound := getQueueNpuLimit(pod, pl.queueLister, fullResourceName); qFound {
			var nsOccupied int64
			for _, usage := range nsLocalPhysicalUsage {
				nsOccupied += usage
			}
			for _, res := range nsLocalReservated {
				nsOccupied += res
			}
			if nsOccupied+int64(reqCount) > queueLimit {
				for nodeName := range nodeFreeNPU {
					if !virtualNodes[nodeName] {
						delete(nodeFreeNPU, nodeName)
					}
				}
				klog.InfoS("ARCSync: queue limit exceeded, removing local nodes",
					"pod", pod.Name, "queueLimit", queueLimit, "nsOccupied", nsOccupied, "required", reqCount)
			}
		}
	} else {
		for _, ni := range nodeInfos {
			node := ni.Node()
			if node != nil && isVirtualNode(node) {
				delete(nodeFreeNPU, node.Name)
			}
		}
	}

	// hasCandidate checks total NPU capacity across all local nodes
	// (regardless of the pod's nodeSelector) plus eligible virtual nodes.
	// Runner pods carry required-npu-count for reservation but may be bound
	// to CPU nodes by the default scheduler — the Reserve plugin stores the
	// reservation on the bound (CPU) node, not the NPU node. By summing
	// (allocatable - nodeTotalOccupied) across ALL nodes, reservations on
	// CPU nodes appear as negative values (0 − reservation) and correctly
	// reduce the cluster-wide total, preventing over-commitment.
	var totalNPUFree int64
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(node) {
			continue
		}
		if isVirtualNode(node) {
			if _, exists := nodeFreeNPU[node.Name]; !exists {
				continue
			}
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		totalNPUFree += allocatable.Value() - nodeTotalOccupied[node.Name]
	}
	hasCandidate := totalNPUFree >= int64(reqCount)

	if !hasCandidate {
		klog.InfoS("ARCSync: PreFilter rejected pod (no node has enough NPU)",
			"pod", pod.Name, "required", reqCount)
		return nil, framework.NewStatus(framework.Unschedulable, "No node has enough available NPU slots")
	}

	if !pl.isOldestPendingRunner(pod, nsHasOffloading) {
		klog.InfoS("ARCSync: FIFO hold — waiting for older runner pods",
			"pod", pod.Name)
		return nil, framework.NewStatus(framework.Unschedulable, "FIFO: waiting for older runner pods to be scheduled first")
	}

	state.Write(stateKey, &preFilterState{
		requiredCount: int64(reqCount),
		resourceName:  fullResourceName,
		nodeFreeNPU:   nodeFreeNPU,
	})
	return nil, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func hasPendingRunnerOnNode(nodeInfos []*framework.NodeInfo, nodeName, resDomain, resModel string) bool {
	for _, ni := range nodeInfos {
		if ni.Node() == nil || ni.Node().Name != nodeName {
			continue
		}
		for _, podInfo := range ni.Pods {
			p := podInfo.Pod
			if p == nil {
				continue
			}
			if p.Labels[RequiredNPUCount] == "" {
				continue
			}
			if p.Labels[ResourceDomain] != resDomain || p.Labels[ResourceModel] != resModel {
				continue
			}
			if p.Status.Phase == v1.PodPending {
				return true
			}
		}
		return false
	}
	return false
}

func (pl *ARCSync) applyLiqoComparison(
	nodeInfos []*framework.NodeInfo,
	pod *v1.Pod,
	resDomain, resModel string,
	fullResourceName v1.ResourceName,
	nsLocalOccupied map[string]int64,
	nodeFreeNPU map[string]int64,
	nsOffloading *unstructured.Unstructured,
	reqCount int64,
	virtualNodes map[string]bool,
) {
	eligibleVirtuals := getEligibleVirtualNodes(nodeInfos, nsOffloading)

	// Remove virtual nodes that are not targeted by the NamespaceOffloading
	// clusterSelector. These must never receive pods from this namespace,
	// regardless of the local-vs-remote comparison outcome. Without this,
	// non-eligible virtual nodes would leak into the candidate set when local
	// wins the comparison or when eligibleVirtuals is empty.
	for nodeName := range nodeFreeNPU {
		if virtualNodes[nodeName] && !eligibleVirtuals[nodeName] {
			delete(nodeFreeNPU, nodeName)
		}
	}

	if len(eligibleVirtuals) == 0 {
		return
	}

	var localTotalAllocatable, localTotalOccupied int64
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || isVirtualNode(node) || !canScheduleOnNode(node) {
			continue
		}
		if _, exists := nodeFreeNPU[node.Name]; !exists {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		localTotalAllocatable += allocatable.Value()
		localTotalOccupied += nsLocalOccupied[node.Name]
	}

	localTotalCapacity := localTotalAllocatable
	if queueLimit, qFound := getQueueNpuLimit(pod, pl.queueLister, fullResourceName); qFound {
		if queueLimit < localTotalCapacity {
			localTotalCapacity = queueLimit
		}
	}

	localRemaining := localTotalCapacity - localTotalOccupied
	if localRemaining < 0 {
		localRemaining = 0
	}

	var bestVirtNode string
	var bestVirtRemaining int64

	localHasCandidate := false
	for nodeName, free := range nodeFreeNPU {
		if !virtualNodes[nodeName] && free >= int64(reqCount) {
			localHasCandidate = true
			break
		}
	}

	for nodeName := range eligibleVirtuals {
		if free, exists := nodeFreeNPU[nodeName]; exists && free > bestVirtRemaining {
			if localHasCandidate && hasPendingRunnerOnNode(nodeInfos, nodeName, resDomain, resModel) {
				continue
			}
			bestVirtRemaining = free
			bestVirtNode = nodeName
		}
	}

	if localRemaining >= bestVirtRemaining {
		for nodeName := range eligibleVirtuals {
			delete(nodeFreeNPU, nodeName)
		}
		klog.InfoS("ARCSync: local wins liqo comparison",
			"pod", pod.Name, "localRemaining", localRemaining, "bestVirtRemaining", bestVirtRemaining)
	} else {
		for nodeName := range nodeFreeNPU {
			if nodeName != bestVirtNode {
				delete(nodeFreeNPU, nodeName)
			}
		}
		klog.InfoS("ARCSync: virtual node wins liqo comparison",
			"pod", pod.Name, "bestVirtNode", bestVirtNode, "bestVirtRemaining", bestVirtRemaining,
			"localRemaining", localRemaining)
	}
}

func (pl *ARCSync) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	nodeName := nodeInfo.Node().Name
	free, exists := data.nodeFreeNPU[nodeName]
	if !exists {
		return framework.NewStatus(framework.Unschedulable, "node not eligible for NPU scheduling")
	}
	// Runner pods carry required-npu-count for reservation but don't request
	// NPU in their containers — they run on CPU nodes while NPU is reserved
	// elsewhere. Only enforce NPU capacity for pods that actually request NPU
	// resources in their containers; for others, let the default scheduler's
	// NodeResourcesFit and NodeAffinity filters handle placement.
	if !podRequestsNPU(pod, data.resourceName) {
		return framework.NewStatus(framework.Success, "")
	}
	if free < data.requiredCount {
		return framework.NewStatus(framework.Unschedulable, "insufficient NPU on node")
	}
	return framework.NewStatus(framework.Success, "")
}

func podRequestsNPU(pod *v1.Pod, resourceName v1.ResourceName) bool {
	for _, container := range pod.Spec.Containers {
		if _, exists := container.Resources.Requests[resourceName]; exists {
			return true
		}
	}
	return false
}

func (pl *ARCSync) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	s, err := state.Read(stateKey)
	if err != nil {
		return 0, framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)
	free := data.nodeFreeNPU[nodeName]
	score := free
	if score > framework.MaxNodeScore {
		score = framework.MaxNodeScore
	}
	if score < 0 {
		score = 0
	}
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (pl *ARCSync) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return framework.NewStatus(framework.Success, "")
	}
	data := s.(*preFilterState)

	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}

	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.inFlightReservations[podKey] = reservation{
		nodeName:  nodeName,
		count:     data.requiredCount,
		timestamp: time.Now(),
		baseName:  getBaseName(pod.Name),
		namespace: pod.Namespace,
	}
	klog.InfoS("ARCSync: Reserved NPU slots", "pod", pod.Name, "node", nodeName, "count", data.requiredCount)
	return framework.NewStatus(framework.Success, "")
}

func (pl *ARCSync) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	podKey := string(pod.UID)
	if podKey == "" {
		podKey = pod.Namespace + "/" + pod.Name
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.inFlightReservations, podKey)
	klog.InfoS("ARCSync: Unreserved NPU slots", "pod", pod.Name, "node", nodeName)
}

func (pl *ARCSync) PostBind(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	s, err := state.Read(stateKey)
	if err != nil {
		return
	}
	data := s.(*preFilterState)

	// Only clear reservation if the pod itself requests NPU (i.e. it's a workflow pod).
	// Runner pods don't request NPU — their reservation must persist until the
	// workflow pod starts and physical usage takes over.
	for _, container := range pod.Spec.Containers {
		if _, exists := container.Resources.Requests[data.resourceName]; exists {
			podKey := string(pod.UID)
			if podKey == "" {
				podKey = pod.Namespace + "/" + pod.Name
			}
			pl.mu.Lock()
			defer pl.mu.Unlock()
			delete(pl.inFlightReservations, podKey)
			klog.InfoS("ARCSync: PostBind cleared reservation (pod has NPU request)", "pod", pod.Name, "node", nodeName)
			return
		}
	}
	klog.V(4).InfoS("ARCSync: PostBind keeping reservation (runner pod)", "pod", pod.Name, "node", nodeName)
}
