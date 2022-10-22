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
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

/*
 * This is needed because of https://github.com/kubernetes/kubernetes/issues/110660
 * Fixed in kubernetes 1.25.0, but enhancements are not backported.
 * So we need to wait until the scheduler plugins repo consumes k8s >= 1.25.0, then
 * We can just add a client-go/cache Indexer and get rid of this workaround:
 *
 * func newNodeNameIndexer(handle framework.Handle) (IndexGetter, error) {
 *     podInformer := handle.SharedInformerFactory().Core().V1().Pods()
 *     err = podInformer.Informer().GetIndexer().AddIndexers(cache.Indexers{
 *         NodeNameIndexer: indexByPodNodeName,
 *     })
 *     if err != nil {
 *     		return nil, err
 *     }
 *     return podInformer.Informer().GetIndexer(), nil
 * }
 * (possibly inlined in the calling site)
 */

// TODO: tests using multiple goroutines (+ race detector)

type NodeIndexer interface {
	GetPodNamespacedNamesByNode(logID, nodeName string) ([]types.NamespacedName, error)
}

type nodeNameIndexer struct {
	rwLock        sync.RWMutex
	nodeToPodsMap map[string]map[types.UID]types.NamespacedName
}

func NewNodeNameIndexer(podInformer k8scache.SharedInformer) NodeIndexer {
	nni := &nodeNameIndexer{
		nodeToPodsMap: make(map[string]map[types.UID]types.NamespacedName),
	}
	podInformer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				klog.V(3).InfoS("nrtcache: nni: add unsupported object %T", obj)
				return
			}
			nni.addPod(pod)
		},
		// Updates are not yet supported
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				klog.V(3).InfoS("nrtcache: nni: delete unsupported object %T", obj)
				return
			}
			nni.deletePod(pod)
		},
	})
	return nni
}

func (nni *nodeNameIndexer) GetPodNamespacedNamesByNode(logID, nodeName string) ([]types.NamespacedName, error) {
	nni.rwLock.RLock()
	defer nni.rwLock.RUnlock()
	nns, ok := nni.nodeToPodsMap[nodeName]
	if !ok {
		klog.V(6).InfoS("nrtcache: nni GET ", "logID", logID, "node", nodeName, "pods", "NONE")
		return []types.NamespacedName{}, nil
	}

	objs := make([]types.NamespacedName, 0, len(nns))
	for _, nname := range nns {
		objs = append(objs, nname)
	}
	klog.V(6).InfoS("nrtcache: nni GET ", "logID", logID, "node", nodeName, "pods", namespacedNameListToString(objs))
	return objs, nil
}

func (nni *nodeNameIndexer) addPod(pod *corev1.Pod) {
	if pod.Spec.NodeName == "" {
		klog.V(4).InfoS("nrtcache: nni WARN", "logID", klog.KObj(pod), "node", "EMPTY", "podUID", pod.UID, "phase", pod.Status.Phase)
		return
	}
	if pod.Status.Phase != corev1.PodRunning {
		// We should only consider pod which are consuming resources. But it's also paramount to take into account what the
		// node side can do. The primary source of data for node agents is the podresources API, and the podresources API
		// cannot do any filtering based on pod runtime status. Accessing this data from other sources (e.g. apiserver, runtime)
		// is either hard (runtime) or should be avoided for performance reasons (apiserver). So the only compromise is
		// to overshoot and - incorrectly - account for ALL the pods reported on the node. Let's at least acknowledge
		// this fact and be pretty loud when it occurs.
		klog.V(4).InfoS("nrtcache: nni WARN", "logID", klog.KObj(pod), "node", pod.Spec.NodeName, "podUID", pod.UID, "phase", pod.Status.Phase)
		// intentional fallback
	}

	nni.rwLock.Lock()
	defer nni.rwLock.Unlock()
	pods, ok := nni.nodeToPodsMap[pod.Spec.NodeName]
	if !ok {
		pods = make(map[types.UID]types.NamespacedName)
		nni.nodeToPodsMap[pod.Spec.NodeName] = pods
	}
	klog.V(6).InfoS("nrtcache: nni ADD ", "logID", klog.KObj(pod), "node", pod.Spec.NodeName, "podUID", pod.UID)
	pods[pod.UID] = types.NamespacedName{
		Namespace: pod.GetNamespace(),
		Name:      pod.GetName(),
	}
}

func (nni *nodeNameIndexer) deletePod(pod *corev1.Pod) {
	nni.rwLock.Lock()
	defer nni.rwLock.Unlock()
	// so maps in golang (up to 1.19 included) do NOT release the memory for the buckets even on delete
	// of their elements. We use maps -even nested- heavily, so are we at risk of leaking memory and
	// being eventually OOM-killed?
	// *YES*. But we can manage.
	// First of all, the amount of nodes in a cluster is expected to be stable.
	// OTOH, we can expect pod churn and a non-negligible amount of intermixed add/delete. This means the inner
	// maps can leak. We can't estimate how frequent or how bad this can be yet.
	// if this becomes a real bug, the fix is to recreate the inner maps with only the live keys, dropping
	// the old leaky map. We should probably do this here, on delete, but not every time but once the pod
	// churn (easily trackable with an integer counting the deletes) crosses a threshold, whose value is TBD.
	//
	// for more details: https://github.com/golang/go/issues/20135
	delete(nni.nodeToPodsMap[pod.Spec.NodeName], pod.UID)
	klog.V(6).InfoS("nrtcache: nni DEL ", "logID", klog.KObj(pod), "node", pod.Spec.NodeName, "podUID", pod.UID)
}

func namespacedNameListToString(nns []types.NamespacedName) string {
	var sb strings.Builder
	if len(nns) > 0 {
		sb.WriteString(nns[0].String())
	}
	for idx := 1; idx < len(nns); idx++ {
		sb.WriteString(", " + nns[idx].String())
	}
	return sb.String()
}
