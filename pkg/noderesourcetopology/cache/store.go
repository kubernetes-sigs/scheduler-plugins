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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/go-logr/logr"
	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	topologyv1alpha2attr "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/attribute"
	"github.com/k8stopologyawareschedwg/podfingerprint"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// nrtStore maps the NRT data by node name. It is not thread safe and needs to be protected by a lock.
// data is intentionally copied each time it enters and exists the store. E.g, no pointer sharing.
type nrtStore struct {
	data map[string]*topologyv1alpha2.NodeResourceTopology
	lh   logr.Logger
}

// newNrtStore creates a new nrtStore and initializes it with copies of the provided Node Resource Topology data.
func newNrtStore(lh logr.Logger, nrts []topologyv1alpha2.NodeResourceTopology) *nrtStore {
	data := make(map[string]*topologyv1alpha2.NodeResourceTopology, len(nrts))
	for _, nrt := range nrts {
		data[nrt.Name] = nrt.DeepCopy()
	}
	lh.V(6).Info("initialized nrtStore", "objects", len(data))
	return &nrtStore{
		data: data,
		lh:   lh,
	}
}

func (nrs nrtStore) Contains(nodeName string) bool {
	_, ok := nrs.data[nodeName]
	return ok
}

// GetNRTCopyByNodeName returns a copy of the stored Node Resource Topology data for the given node,
// or nil if no data is associated to that node.
func (nrs *nrtStore) GetNRTCopyByNodeName(nodeName string) *topologyv1alpha2.NodeResourceTopology {
	obj, ok := nrs.data[nodeName]
	if !ok {
		nrs.lh.V(3).Info("missing cached NodeTopology", "node", nodeName)
		return nil
	}
	return obj.DeepCopy()
}

// Update adds or replace the Node Resource Topology associated to a node. Always do a copy.
func (nrs *nrtStore) Update(nrt *topologyv1alpha2.NodeResourceTopology) {
	nrs.data[nrt.Name] = nrt.DeepCopy()
	nrs.lh.V(5).Info("updated cached NodeTopology", "node", nrt.Name)
}

// resourceStore maps the resource requested by pod by pod namespaed name. It is not thread safe and needs to be protected by a lock.
type resourceStore struct {
	// key: namespace + "/" name
	data map[string]corev1.ResourceList
	lh   logr.Logger
}

func newResourceStore(lh logr.Logger) *resourceStore {
	return &resourceStore{
		data: make(map[string]corev1.ResourceList),
		lh:   lh,
	}
}

func (rs *resourceStore) String() string {
	var sb strings.Builder
	for podKey, podRes := range rs.data {
		sb.WriteString(podKey + "::[" + stringify.ResourceList(podRes) + "];")
	}
	return sb.String()
}

// AddPod returns true if updating existing pod, false if adding for the first time
func (rs *resourceStore) AddPod(pod *corev1.Pod) bool {
	key := pod.Namespace + "/" + pod.Name
	_, ok := rs.data[key]
	if ok {
		// should not happen, so we log with a low level
		rs.lh.V(4).Info("updating existing entry", "key", key)
	}
	resData := util.GetPodEffectiveRequest(pod)
	rs.lh.V(5).Info("resourcestore ADD", stringify.ResourceListToLoggable(resData)...)
	rs.data[key] = resData
	return ok
}

// DeletePod returns true if deleted an existing pod, false otherwise
func (rs *resourceStore) DeletePod(pod *corev1.Pod) bool {
	key := pod.Namespace + "/" + pod.Name
	_, ok := rs.data[key]
	if ok {
		// should not happen, so we log with a low level
		rs.lh.V(4).Info("removing missing entry", "key", key)
	}
	rs.lh.V(5).Info("resourcestore DEL", stringify.ResourceListToLoggable(rs.data[key])...)
	delete(rs.data, key)
	return ok
}

// UpdateNRT updates the provided Node Resource Topology object with the resources tracked in this store,
// performing pessimistic overallocation across all the NUMA zones.
func (rs *resourceStore) UpdateNRT(nrt *topologyv1alpha2.NodeResourceTopology, logKeysAndValues ...any) {
	for key, res := range rs.data {
		// We cannot predict on which Zone the workload will be placed.
		// And we should totally not guess. So the only safe (and conservative)
		// choice is to decrement the available resources from *all* the zones.
		// This can cause false negatives, but will never cause false positives,
		// which are much worse.
		for zi := 0; zi < len(nrt.Zones); zi++ {
			zone := &nrt.Zones[zi] // shortcut
			for ri := 0; ri < len(zone.Resources); ri++ {
				zr := &zone.Resources[ri] // shortcut
				qty, ok := res[corev1.ResourceName(zr.Name)]
				if !ok {
					// this is benign; it is totally possible some resources are not
					// available on some zones (think PCI devices), hence we don't
					// even report this error, being an expected condition
					continue
				}
				if zr.Available.Cmp(qty) < 0 {
					// this should happen rarely, and it is likely caused by
					// a bug elsewhere.
					logKeysAndValues = append(logKeysAndValues, "zone", zr.Name, logging.KeyNode, nrt.Name, "available", zr.Available, "requestor", key, "quantity", qty.String())
					rs.lh.V(3).Info("cannot decrement resource", logKeysAndValues...)
					zr.Available = resource.Quantity{}
					continue
				}

				zr.Available.Sub(qty)
			}
		}
	}
}

type counter map[string]int

func newCounter() counter {
	return make(map[string]int)
}

func (cnt counter) Incr(key string) int {
	cnt[key]++
	return cnt[key]
}

func (cnt counter) IsSet(key string) bool {
	_, ok := cnt[key]
	return ok
}

func (cnt counter) Delete(key string) {
	delete(cnt, key)
}

func (cnt counter) Keys() []string {
	keys := make([]string, 0, len(cnt))
	for key := range cnt {
		keys = append(keys, key)
	}
	return keys
}

func (cnt counter) Clone() counter {
	cloned := make(map[string]int)
	for key, val := range cnt {
		cloned[key] = val
	}
	return cloned
}

func (cnt counter) Len() int {
	return len(cnt)
}

// podFingerprintForNodeTopology extracts without recomputing the pods fingerprint from
// the provided Node Resource Topology object. Returns the expected fingerprint and the method to compute it.
func podFingerprintForNodeTopology(nrt *topologyv1alpha2.NodeResourceTopology, method apiconfig.CacheResyncMethod) (string, bool) {
	wantsOnlyExclRes := false
	if attr, ok := topologyv1alpha2attr.Get(nrt.Attributes, podfingerprint.Attribute); ok {
		if method == apiconfig.CacheResyncOnlyExclusiveResources {
			wantsOnlyExclRes = true
		} else if method == apiconfig.CacheResyncAutodetect {
			attrMethod, ok := topologyv1alpha2attr.Get(nrt.Attributes, podfingerprint.AttributeMethod)
			if ok && (attrMethod.Value == podfingerprint.MethodWithExclusiveResources) {
				wantsOnlyExclRes = true
			}
		}
		return attr.Value, wantsOnlyExclRes
	}
	// with legacy annotations, the exclusive resource method can't ever be true. Just hardcode false.
	if nrt.Annotations != nil {
		return nrt.Annotations[podfingerprint.Annotation], false
	}
	return "", false
}

type podData struct {
	Namespace             string
	Name                  string
	HasExclusiveResources bool
}

// checkPodFingerprintForNode verifies if the given pods fingeprint (usually from NRT update) matches the
// computed one using the stored data about pods running on nodes. Returns nil on success, or an error
// describing the failure
func checkPodFingerprintForNode(lh logr.Logger, objs []podData, nodeName, pfpExpected string, onlyExclRes bool) error {
	st := podfingerprint.MakeStatus(nodeName)
	pfp := podfingerprint.NewTracingFingerprint(len(objs), &st)
	for _, obj := range objs {
		if onlyExclRes && !obj.HasExclusiveResources {
			continue
		}
		pfp.Add(obj.Namespace, obj.Name)
	}
	pfpComputed := pfp.Sign()

	lh.V(4).Info("podset fingerprint check", "expected", pfpExpected, "computed", pfpComputed, "onlyExclusiveResources", onlyExclRes)
	lh.V(6).Info("podset fingerprint debug", "status", st.Repr())

	err := pfp.Check(pfpExpected)
	podfingerprint.MarkCompleted(st)
	return err
}
