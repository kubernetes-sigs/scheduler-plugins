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

package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/podfingerprint"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/podprovider"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

type OverReserve struct {
	client           ctrlclient.Client
	lock             sync.Mutex
	nrts             *nrtStore
	assumedResources map[string]*resourceStore // nodeName -> resourceStore
	// nodesMaybeOverreserved counts how many times a node is filtered out. This is used as trigger condition to try
	// to resync nodes. See The documentation of Resync() below for more details.
	nodesMaybeOverreserved counter
	nodesWithForeignPods   counter
	podLister              podlisterv1.PodLister
	resyncMethod           apiconfig.CacheResyncMethod
	isPodRelevant          podprovider.PodFilterFunc
}

func NewOverReserve(cfg *apiconfig.NodeResourceTopologyCache, client ctrlclient.Client, podLister podlisterv1.PodLister, isPodRelevant podprovider.PodFilterFunc) (*OverReserve, error) {
	if client == nil || podLister == nil {
		return nil, fmt.Errorf("nrtcache: received nil references")
	}

	resyncMethod := getCacheResyncMethod(cfg)

	nrtObjs := &topologyv1alpha2.NodeResourceTopologyList{}
	// TODO: we should pass-in a context in the future
	if err := client.List(context.Background(), nrtObjs); err != nil {
		return nil, err
	}

	klog.V(3).InfoS("nrtcache: initializing", "objects", len(nrtObjs.Items), "method", resyncMethod)
	obj := &OverReserve{
		client:                 client,
		nrts:                   newNrtStore(nrtObjs.Items),
		assumedResources:       make(map[string]*resourceStore),
		nodesMaybeOverreserved: newCounter(),
		nodesWithForeignPods:   newCounter(),
		podLister:              podLister,
		resyncMethod:           resyncMethod,
		isPodRelevant:          isPodRelevant,
	}
	return obj, nil
}

func (ov *OverReserve) GetCachedNRTCopy(ctx context.Context, nodeName string, pod *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, bool) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	if ov.nodesWithForeignPods.IsSet(nodeName) {
		return nil, false
	}

	nrt := ov.nrts.GetNRTCopyByNodeName(nodeName)
	if nrt == nil {
		return nil, true
	}
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		return nrt, true
	}

	klog.V(6).InfoS("nrtcache NRT", "logID", klog.KObj(pod), "vanilla", stringify.NodeResourceTopologyResources(nrt))
	nodeAssumedResources.UpdateNRT(klog.KObj(pod).String(), nrt)

	klog.V(5).InfoS("nrtcache NRT", "logID", klog.KObj(pod), "updated", stringify.NodeResourceTopologyResources(nrt))
	return nrt, true
}

func (ov *OverReserve) NodeMaybeOverReserved(nodeName string, pod *corev1.Pod) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	val := ov.nodesMaybeOverreserved.Incr(nodeName)
	klog.V(4).InfoS("nrtcache: mark discarded", "logID", klog.KObj(pod), "node", nodeName, "count", val)
}

func (ov *OverReserve) NodeHasForeignPods(nodeName string, pod *corev1.Pod) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	if !ov.nrts.Contains(nodeName) {
		klog.V(5).InfoS("nrtcache: ignoring foreign pods", "logID", klog.KObj(pod), "node", nodeName, "nrtinfo", "missing")
		return
	}
	val := ov.nodesWithForeignPods.Incr(nodeName)
	klog.V(4).InfoS("nrtcache: marked with foreign pods", "logID", klog.KObj(pod), "node", nodeName, "count", val)
}

func (ov *OverReserve) ReserveNodeResources(nodeName string, pod *corev1.Pod) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		nodeAssumedResources = newResourceStore()
		ov.assumedResources[nodeName] = nodeAssumedResources
	}

	nodeAssumedResources.AddPod(pod)
	klog.V(5).InfoS("nrtcache post reserve", "logID", klog.KObj(pod), "node", nodeName, "assumedResources", nodeAssumedResources.String())

	ov.nodesMaybeOverreserved.Delete(nodeName)
	klog.V(6).InfoS("nrtcache: reset discard counter", "logID", klog.KObj(pod), "node", nodeName)
}

func (ov *OverReserve) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		// this should not happen, so we're vocal about it
		// we don't return error because not much to do to recover anyway
		klog.V(3).InfoS("nrtcache: no resources tracked", "logID", klog.KObj(pod), "node", nodeName)
		return
	}

	nodeAssumedResources.DeletePod(pod)
	klog.V(5).InfoS("nrtcache post release", "logID", klog.KObj(pod), "node", nodeName, "assumedResources", nodeAssumedResources.String())
}

// GetNodesOverReservationStatus returns a slice of all the node names which have been discarded previously,
// so which are supposed to be `dirty` in the cache, and a slice with all the other known nodes, which are expected to be clean.
// The union of the returned slices must be the total set of known nodes.
// A node can be discarded for two reasons:
// 1. it legitmately cannot fit containers because it has not enough free resources
// 2. it was pessimistically overallocated, so the node is a candidate for resync
// This function enables the caller to know the slice of nodes should be considered for resync,
// avoiding the need to rescan the full node list.
func (ov *OverReserve) GetNodesOverReservationStatus(logID string) ([]string, []string) {
	ov.lock.Lock()
	defer ov.lock.Unlock()

	allNodes := sets.New[string](ov.nrts.NodeNames()...)
	// this is intentionally aggressive. We don't yet make any attempt to find out if the
	// node was discarded because pessimistically overrserved (which should indeed trigger
	// a resync) or if it was discarded because the actual resources on the node really were
	// exhausted. We do like this because this is the safest approach. We will optimize
	// the node selection logic later on to make the resync procedure less aggressive but
	// still correct.
	dirtyNodes := ov.nodesWithForeignPods.Clone()
	foreignCount := dirtyNodes.Len()

	for _, node := range ov.nodesMaybeOverreserved.Keys() {
		dirtyNodes.Incr(node)
	}

	dirtyList := dirtyNodes.Keys()
	if len(dirtyList) > 0 {
		klog.V(4).InfoS("nrtcache: found dirty nodes", "logID", logID, "foreign", foreignCount, "discarded", len(dirtyList)-foreignCount, "total", len(dirtyList), "allNodes", len(allNodes))
	}

	cleanList := allNodes.Delete(dirtyList...).UnsortedList()
	if len(cleanList) > 0 {
		klog.V(4).InfoS("nrtcache: found clean nodes", "logID", logID, "dirtyCount", len(dirtyList), "total", len(cleanList), "allNodes", len(allNodes))
	}

	return dirtyList, cleanList
}

// Resync implements the cache resync loop step. This function checks if the latest available NRT information received matches the
// state of a dirty node, for all the dirty nodes. If this is the case, the cache of a node can be Flush()ed.
// The trigger for attempting to resync a node is not just that we overallocated it. If a node was overallocated but still has capacity,
// we keep using it. But we cannot predict when the capacity is too low, because that would mean predicting the future workload requests.
// The best heuristic found so far is count how many times the node was skipped *AND* crosscheck with its overallocation state.
// If *both* a node has pessimistic overallocation accounted to it *and* was discarded "too many" (how much is too much is a runtime parameter
// which needs to be set and tuned) times, then it becomes a candidate for resync. Just using one of these two factors would lead to
// too aggressive resync attempts, so to more, likely unnecessary, computation work on the scheduler side.
func (ov *OverReserve) Resync() {
	// we are not working with a specific pod, so we need a unique key to track this flow
	logID := logIDFromTime()

	klog.V(6).InfoS("nrtcache: resync NodeTopology cache starting", "logID", logID)
	defer klog.V(6).InfoS("nrtcache: resync NodeTopology cache complete", "logID", logID)

	dirtyNodes, cleanNodes := ov.GetNodesOverReservationStatus(logID)
	dirtyUpdates := ov.computeResyncByPods(logID, dirtyNodes)
	cleanUpdates := ov.makeResyncMap(logID, cleanNodes)

	ov.FlushNodes(logID, dirtyUpdates, cleanUpdates)
}

// computeResyncByPods must not require additiona locking for the `OverReserve` data structures
func (ov *OverReserve) computeResyncByPods(logID string, nodeNames []string) map[string]*topologyv1alpha2.NodeResourceTopology {
	nrtUpdatesMap := make(map[string]*topologyv1alpha2.NodeResourceTopology)

	// node -> pod identifier (namespace, name)
	nodeToObjsMap, err := makeNodeToPodDataMap(ov.podLister, ov.isPodRelevant, logID)
	if err != nil {
		klog.ErrorS(err, "cannot find the mapping between running pods and nodes")
		return nrtUpdatesMap
	}

	for _, nodeName := range nodeNames {
		nrtCandidate := &topologyv1alpha2.NodeResourceTopology{}
		if err := ov.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, nrtCandidate); err != nil {
			klog.V(3).InfoS("nrtcache: failed to get NodeTopology", "logID", logID, "node", nodeName, "error", err)
			continue
		}
		if nrtCandidate == nil {
			klog.V(3).InfoS("nrtcache: missing NodeTopology", "logID", logID, "node", nodeName)
			continue
		}

		objs, ok := nodeToObjsMap[nodeName]
		if !ok {
			// this really should never happen
			klog.V(3).InfoS("nrtcache: cannot find any pod for node", "logID", logID, "node", nodeName)
			continue
		}

		pfpExpected, onlyExclRes := podFingerprintForNodeTopology(nrtCandidate, ov.resyncMethod)
		if pfpExpected == "" {
			klog.V(3).InfoS("nrtcache: missing NodeTopology podset fingerprint data", "logID", logID, "node", nodeName)
			continue
		}

		klog.V(6).InfoS("nrtcache: trying to resync NodeTopology", "logID", logID, "node", nodeName, "fingerprint", pfpExpected, "onlyExclusiveResources", onlyExclRes)

		err = checkPodFingerprintForNode(logID, objs, nodeName, pfpExpected, onlyExclRes)
		if errors.Is(err, podfingerprint.ErrSignatureMismatch) {
			// can happen, not critical
			klog.V(5).InfoS("nrtcache: NodeTopology podset fingerprint mismatch", "logID", logID, "node", nodeName)
			continue
		}
		if err != nil {
			// should never happen, let's be vocal
			klog.V(3).ErrorS(err, "nrtcache: checking NodeTopology podset fingerprint", "logID", logID, "node", nodeName)
			continue
		}

		klog.V(4).InfoS("nrtcache: overriding cached info", "logID", logID, "node", nodeName)
		nrtUpdatesMap[nodeName] = nrtCandidate
	}

	return nrtUpdatesMap
}

func (ov *OverReserve) makeResyncMap(logID string, nodeNames []string) map[string]*topologyv1alpha2.NodeResourceTopology {
	nrtUpdatesMap := make(map[string]*topologyv1alpha2.NodeResourceTopology)

	for _, nodeName := range nodeNames {
		nrtCandidate := &topologyv1alpha2.NodeResourceTopology{}
		err := ov.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, nrtCandidate)
		if err != nil {
			klog.V(3).InfoS("nrtcache: failed to get NodeTopology", "logID", logID, "node", nodeName, "error", err)
			continue
		}
		if nrtCandidate == nil {
			klog.V(3).InfoS("nrtcache: missing NodeTopology", "logID", logID, "node", nodeName)
			continue
		}

		klog.V(4).InfoS("nrtcache: overriding cached info", "logID", logID, "node", nodeName)
		nrtUpdatesMap[nodeName] = nrtCandidate
	}

	return nrtUpdatesMap
}

// FlushNodes drops all the cached information about a given node, resetting its state clean.
func (ov *OverReserve) FlushNodes(logID string, dirtyUpdates, cleanUpdates map[string]*topologyv1alpha2.NodeResourceTopology) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	for _, nrt := range dirtyUpdates {
		klog.V(4).InfoS("nrtcache: flushing", "logID", logID, "node", nrt.Name)
		ov.nrts.Update(nrt)
		delete(ov.assumedResources, nrt.Name)
		ov.nodesMaybeOverreserved.Delete(nrt.Name)
		ov.nodesWithForeignPods.Delete(nrt.Name)
	}
	for _, nrt := range cleanUpdates {
		klog.V(4).InfoS("nrtcache: refreshing", "logID", logID, "node", nrt.Name)
		ov.nrts.UpdateIfNeeded(nrt, areAttrsChanged)
	}
}

func areAttrsChanged(oldNrt, newNrt *topologyv1alpha2.NodeResourceTopology) bool {
	oldConf := nodeconfig.TopologyManagerFromNodeResourceTopology(oldNrt)
	newConf := nodeconfig.TopologyManagerFromNodeResourceTopology(newNrt)
	return !oldConf.Equal(newConf)
}

// to be used only in tests
func (ov *OverReserve) Store() *nrtStore {
	return ov.nrts
}

func makeNodeToPodDataMap(podLister podlisterv1.PodLister, isPodRelevant podprovider.PodFilterFunc, logID string) (map[string][]podData, error) {
	nodeToObjsMap := make(map[string][]podData)
	pods, err := podLister.List(labels.Everything())
	if err != nil {
		return nodeToObjsMap, err
	}
	for _, pod := range pods {
		if !isPodRelevant(pod, logID) {
			continue
		}
		nodeObjs := nodeToObjsMap[pod.Spec.NodeName]
		nodeObjs = append(nodeObjs, podData{
			Namespace:             pod.Namespace,
			Name:                  pod.Name,
			HasExclusiveResources: resourcerequests.AreExclusiveForPod(pod),
		})
		nodeToObjsMap[pod.Spec.NodeName] = nodeObjs
	}
	return nodeToObjsMap, nil
}

func logIDFromTime() string {
	return fmt.Sprintf("resync%v", time.Now().UnixMilli())
}

func getCacheResyncMethod(cfg *apiconfig.NodeResourceTopologyCache) apiconfig.CacheResyncMethod {
	var resyncMethod apiconfig.CacheResyncMethod
	if cfg != nil && cfg.ResyncMethod != nil {
		resyncMethod = *cfg.ResyncMethod
	} else { // explicitly set to nil?
		resyncMethod = apiconfig.CacheResyncAutodetect
		klog.InfoS("cache resync method missing", "fallback", resyncMethod)
	}
	return resyncMethod
}

func (ov *OverReserve) PostBind(nodeName string, pod *corev1.Pod) {}
