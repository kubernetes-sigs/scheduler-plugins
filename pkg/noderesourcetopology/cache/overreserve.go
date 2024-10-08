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

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/podfingerprint"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/podprovider"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/resourcerequests"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

type OverReserve struct {
	lh               logr.Logger
	client           ctrlclient.Reader
	lock             sync.Mutex
	generation       uint64
	nrts             *nrtStore
	assumedResources map[string]*resourceStore // nodeName -> resourceStore
	// nodesMaybeOverreserved counts how many times a node is filtered out. This is used as trigger condition to try
	// to resync nodes. See The documentation of Resync() below for more details.
	nodesMaybeOverreserved counter
	nodesWithForeignPods   counter
	nodesWithAttrUpdate    counter
	podLister              podlisterv1.PodLister
	resyncMethod           apiconfig.CacheResyncMethod
	resyncScope            apiconfig.CacheResyncScope
	isPodRelevant          podprovider.PodFilterFunc
}

func NewOverReserve(ctx context.Context, lh logr.Logger, cfg *apiconfig.NodeResourceTopologyCache, client ctrlclient.WithWatch, podLister podlisterv1.PodLister, isPodRelevant podprovider.PodFilterFunc) (*OverReserve, error) {
	if client == nil || podLister == nil {
		return nil, fmt.Errorf("received nil references")
	}

	nrtObjs := &topologyv1alpha2.NodeResourceTopologyList{}
	if err := client.List(ctx, nrtObjs); err != nil {
		return nil, err
	}

	resyncMethod := getCacheResyncMethod(lh, cfg)
	resyncScope := getCacheResyncScope(lh, cfg)

	lh.V(2).Info("initializing", "noderesourcetopologies", len(nrtObjs.Items), "method", resyncMethod, "scope", resyncScope)
	obj := &OverReserve{
		lh:                     lh,
		client:                 client,
		nrts:                   newNrtStore(lh, nrtObjs.Items),
		assumedResources:       make(map[string]*resourceStore),
		nodesMaybeOverreserved: newCounter(),
		nodesWithForeignPods:   newCounter(),
		nodesWithAttrUpdate:    newCounter(),
		podLister:              podLister,
		resyncMethod:           resyncMethod,
		isPodRelevant:          isPodRelevant,
	}

	if resyncScope == apiconfig.CacheResyncScopeAll {
		wt := Watcher{
			lh:    obj.lh,
			nrts:  obj.nrts,
			nodes: obj.nodesWithAttrUpdate,
		}
		go wt.NodeResourceTopologies(ctx, client)
	}

	return obj, nil
}

func (ov *OverReserve) GetCachedNRTCopy(ctx context.Context, nodeName string, pod *corev1.Pod) (*topologyv1alpha2.NodeResourceTopology, CachedNRTInfo) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	if ov.nodesWithForeignPods.IsSet(nodeName) {
		return nil, CachedNRTInfo{}
	}

	info := CachedNRTInfo{Fresh: true}
	nrt := ov.nrts.GetNRTCopyByNodeName(nodeName)
	if nrt == nil {
		return nil, info
	}

	info.Generation = ov.generation
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		return nrt, info
	}

	logID := klog.KObj(pod)
	lh := ov.lh.WithValues(logging.KeyPod, logID, logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName, logging.KeyGeneration, ov.generation)

	lh.V(6).Info("NRT", "fromcache", stringify.NodeResourceTopologyResources(nrt))
	nodeAssumedResources.UpdateNRT(nrt, logging.KeyPod, logID)

	lh.V(5).Info("NRT", "withassumed", stringify.NodeResourceTopologyResources(nrt))
	return nrt, info
}

func (ov *OverReserve) NodeMaybeOverReserved(nodeName string, pod *corev1.Pod) {
	ov.lock.Lock()
	defer ov.lock.Unlock()
	val := ov.nodesMaybeOverreserved.Incr(nodeName)
	ov.lh.V(4).Info("mark discarded", logging.KeyNode, nodeName, "count", val)
}

func (ov *OverReserve) NodeHasForeignPods(nodeName string, pod *corev1.Pod) {
	lh := ov.lh.WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)
	ov.lock.Lock()
	defer ov.lock.Unlock()
	if !ov.nrts.Contains(nodeName) {
		lh.V(5).Info("ignoring foreign pods", "nrtinfo", "missing")
		return
	}
	val := ov.nodesWithForeignPods.Incr(nodeName)
	lh.V(2).Info("marked with foreign pods", logging.KeyNode, nodeName, "count", val)
}

func (ov *OverReserve) ReserveNodeResources(nodeName string, pod *corev1.Pod) {
	lh := ov.lh.WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)
	ov.lock.Lock()
	defer ov.lock.Unlock()
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		nodeAssumedResources = newResourceStore(ov.lh)
		ov.assumedResources[nodeName] = nodeAssumedResources
	}

	nodeAssumedResources.AddPod(pod)
	lh.V(2).Info("post reserve", logging.KeyNode, nodeName, "assumedResources", nodeAssumedResources.String())

	ov.nodesMaybeOverreserved.Delete(nodeName)
	lh.V(6).Info("reset discard counter", logging.KeyNode, nodeName)
}

func (ov *OverReserve) UnreserveNodeResources(nodeName string, pod *corev1.Pod) {
	lh := ov.lh.WithValues(logging.KeyPod, klog.KObj(pod), logging.KeyPodUID, logging.PodUID(pod), logging.KeyNode, nodeName)
	ov.lock.Lock()
	defer ov.lock.Unlock()
	nodeAssumedResources, ok := ov.assumedResources[nodeName]
	if !ok {
		// this should not happen, so we're vocal about it
		// we don't return error because not much to do to recover anyway
		lh.V(2).Info("no resources tracked", logging.KeyNode, nodeName)
		return
	}

	nodeAssumedResources.DeletePod(pod)
	lh.V(2).Info("post unreserve", logging.KeyNode, nodeName, "assumedResources", nodeAssumedResources.String())
}

type DesyncedNodes struct {
	Generation        uint64
	MaybeOverReserved []string
	ConfigChanged     []string
}

func (rn DesyncedNodes) String() string {
	return fmt.Sprintf("desyncedNodes{MaybeOverReserved: %v, ConfigChanged: %v}", rn.MaybeOverReserved, rn.ConfigChanged)
}

func (rn DesyncedNodes) Len() int {
	return len(rn.MaybeOverReserved) + len(rn.ConfigChanged)
}

func (rn DesyncedNodes) DirtyCount() int {
	return len(rn.MaybeOverReserved)
}

// GetDesyncNodes returns info with all the node names which have been discarded previously,
// so which are supposed to be `dirty` in the cache.
// A node can be discarded for two reasons:
//  1. it legitimately cannot fit containers because it has not enough free resources
//  2. it was pessimistically overallocated, so the node is a candidate for resync
//
// or
//  3. it received a metadata update while at steady state (resource allocation didn't change),
//     typically at idle time (unloaded node)
//
// This function enables the caller to know the slice of nodes should be considered for resync,
// avoiding the need to rescan the full node list.
func (ov *OverReserve) GetDesyncedNodes(lh logr.Logger) DesyncedNodes {
	ov.lock.Lock()
	defer ov.lock.Unlock()

	// make sure to log the generation to be able to crosscorrelate with later logs
	lh = lh.WithValues(logging.KeyGeneration, ov.generation)

	// this is intentionally aggressive. We don't yet make any attempt to find out if the
	// node was discarded because pessimistically overrserved (which should indeed trigger
	// a resync) or if it was discarded because the actual resources on the node really were
	// exhausted. We do like this because this is the safest approach. We will optimize
	// the node selection logic later on to make the resync procedure less aggressive but
	// still correct.
	nodes := ov.nodesWithForeignPods.Clone()
	foreignCount := nodes.Len()

	overreservedCount := ov.nodesMaybeOverreserved.Len()
	for _, node := range ov.nodesMaybeOverreserved.Keys() {
		nodes.Incr(node)
	}

	// always use local copies
	configChangeNodes := ov.nodesWithAttrUpdate.Clone()
	configChangeCount := configChangeNodes.Len()

	if nodes.Len() > 0 {
		lh.V(4).Info("found dirty nodes", "foreign", foreignCount, "discarded", overreservedCount, "configChange", configChangeCount, "total", nodes.Len())
	}
	return DesyncedNodes{
		Generation:        ov.generation,
		MaybeOverReserved: nodes.Keys(),
		ConfigChanged:     configChangeNodes.Keys(),
	}
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
	lh_ := ov.lh.WithName(logging.FlowCacheSync)
	lh_.V(4).Info(logging.FlowBegin)
	defer lh_.V(4).Info(logging.FlowEnd)

	nodes := ov.GetDesyncedNodes(lh_)
	// we start without because chicken/egg problem. This is the earliest we can use the generation value.
	lh_ = lh_.WithValues(logging.KeyGeneration, nodes.Generation)

	// avoid as much as we can unnecessary work and logs.
	if nodes.Len() == 0 {
		lh_.V(5).Info("no dirty nodes detected")
		return
	}

	// node -> pod identifier (namespace, name)
	nodeToObjsMap, err := makeNodeToPodDataMap(lh_, ov.podLister, ov.isPodRelevant)
	if err != nil {
		lh_.Error(err, "cannot find the mapping between running pods and nodes")
		return
	}

	var nrtUpdates []*topologyv1alpha2.NodeResourceTopology
	for _, nodeName := range nodes.MaybeOverReserved {
		lh := lh_.WithValues(logging.KeyNode, nodeName)

		nrtCandidate := &topologyv1alpha2.NodeResourceTopology{}
		if err := ov.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, nrtCandidate); err != nil {
			lh.V(2).Info("failed to get NodeTopology", "error", err)
			continue
		}
		if nrtCandidate == nil {
			lh.V(2).Info("missing NodeTopology")
			continue
		}

		objs, ok := nodeToObjsMap[nodeName]
		if !ok {
			// this really should never happen
			lh.Info("cannot find any pod for node")
			continue
		}

		pfpExpected, onlyExclRes := podFingerprintForNodeTopology(nrtCandidate, ov.resyncMethod)
		if pfpExpected == "" {
			lh.V(2).Info("missing NodeTopology podset fingerprint data")
			continue
		}

		lh.V(4).Info("trying to sync NodeTopology", "fingerprint", pfpExpected, "onlyExclusiveResources", onlyExclRes)

		err = checkPodFingerprintForNode(lh, objs, nodeName, pfpExpected, onlyExclRes)
		if errors.Is(err, podfingerprint.ErrSignatureMismatch) {
			// can happen, not critical
			lh.V(4).Info("NodeTopology podset fingerprint mismatch")
			continue
		}
		if err != nil {
			// should never happen, let's be vocal
			lh.Error(err, "checking NodeTopology podset fingerprint")
			continue
		}

		lh.V(4).Info("overriding cached info", "reason", "resynced")
		nrtUpdates = append(nrtUpdates, nrtCandidate)
	}

	for _, nodeName := range nodes.ConfigChanged {
		lh := lh_.WithValues(logging.KeyNode, nodeName)

		nrtCandidate := &topologyv1alpha2.NodeResourceTopology{}
		if err := ov.client.Get(context.Background(), types.NamespacedName{Name: nodeName}, nrtCandidate); err != nil {
			lh.V(2).Info("failed to get NodeTopology", "error", err)
			continue
		}
		if nrtCandidate == nil {
			lh.V(2).Info("missing NodeTopology")
			continue
		}

		lh.V(4).Info("overriding cached info", "reason", "configChanged")
		nrtUpdates = append(nrtUpdates, nrtCandidate)
	}

	ov.FlushNodes(lh_, nrtUpdates...)
}

// FlushNodes drops all the cached information about a given node, resetting its state clean.
func (ov *OverReserve) FlushNodes(lh logr.Logger, nrts ...*topologyv1alpha2.NodeResourceTopology) {
	ov.lock.Lock()
	defer ov.lock.Unlock()

	for _, nrt := range nrts {
		lh.V(2).Info("flushing", logging.KeyNode, nrt.Name)
		ov.nrts.Update(nrt)
		delete(ov.assumedResources, nrt.Name)
		ov.nodesMaybeOverreserved.Delete(nrt.Name)
		ov.nodesWithForeignPods.Delete(nrt.Name)
		ov.nodesWithAttrUpdate.Delete(nrt.Name)
	}

	if len(nrts) == 0 {
		return
	}

	// increase only if we mutated the internal state
	ov.generation += 1
	lh.V(2).Info("generation", "new", ov.generation)
}

// to be used only in tests
func (ov *OverReserve) Store() *nrtStore {
	return ov.nrts
}

func makeNodeToPodDataMap(lh logr.Logger, podLister podlisterv1.PodLister, isPodRelevant podprovider.PodFilterFunc) (map[string][]podData, error) {
	nodeToObjsMap := make(map[string][]podData)
	pods, err := podLister.List(labels.Everything())
	if err != nil {
		return nodeToObjsMap, err
	}
	for _, pod := range pods {
		if !isPodRelevant(lh, pod) {
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

func getCacheResyncMethod(lh logr.Logger, cfg *apiconfig.NodeResourceTopologyCache) apiconfig.CacheResyncMethod {
	var resyncMethod apiconfig.CacheResyncMethod
	if cfg != nil && cfg.ResyncMethod != nil {
		resyncMethod = *cfg.ResyncMethod
	} else { // explicitly set to nil?
		resyncMethod = apiconfig.CacheResyncAutodetect
		lh.Info("cache resync method missing", "fallback", resyncMethod)
	}
	return resyncMethod
}

func getCacheResyncScope(lh logr.Logger, cfg *apiconfig.NodeResourceTopologyCache) apiconfig.CacheResyncScope {
	var resyncScope apiconfig.CacheResyncScope
	if cfg != nil && cfg.ResyncScope != nil {
		resyncScope = *cfg.ResyncScope
	} else { // explicitly set to nil?
		resyncScope = apiconfig.CacheResyncScopeAll
		lh.Info("cache resync scope missing", "fallback", resyncScope)
	}
	return resyncScope
}

func (ov *OverReserve) PostBind(nodeName string, pod *corev1.Pod) {}
