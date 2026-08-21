package arcsync

import (
	"encoding/json"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const virtNodeLabelKey = "liqo.io/remote-cluster-id"

var (
	gvrNamespaceOffloading = schema.GroupVersionResource{
		Group:    "offloading.liqo.io",
		Version:  "v1beta1",
		Resource: "namespaceoffloadings",
	}
	gvrQueue = schema.GroupVersionResource{
		Group:    "scheduling.volcano.sh",
		Version:  "v1beta1",
		Resource: "queues",
	}
)

type nsOffloadingLister interface {
	Get(namespace string) (*unstructured.Unstructured, bool, error)
}

type queueLister interface {
	Get(queueName string) (*unstructured.Unstructured, bool, error)
}

type dynamicNSOffloadingLister struct {
	lister cache.GenericLister
}

func (d *dynamicNSOffloadingLister) Get(namespace string) (*unstructured.Unstructured, bool, error) {
	objs, err := d.lister.List(labels.Everything())
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if u.GetNamespace() == namespace {
			return u, true, nil
		}
	}
	return nil, false, nil
}

type dynamicQueueLister struct {
	lister cache.GenericLister
}

func (d *dynamicQueueLister) Get(queueName string) (*unstructured.Unstructured, bool, error) {
	obj, err := d.lister.Get(queueName)
	if err != nil {
		return nil, false, nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}
	return u, true, nil
}

func isVirtualNode(node *v1.Node) bool {
	if node == nil {
		return false
	}
	_, exists := node.Labels[virtNodeLabelKey]
	return exists
}

func calcVirtualNodeOccupied(nodeInfo *framework.NodeInfo, resDomain, resModel string) int64 {
	if nodeInfo == nil {
		return 0
	}
	var occupied int64
	for _, podInfo := range nodeInfo.Pods {
		p := podInfo.Pod
		if p == nil {
			continue
		}
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		if p.Labels[RequiredNPUCount] == "" {
			continue
		}
		if p.Labels[ResourceDomain] != resDomain || p.Labels[ResourceModel] != resModel {
			continue
		}
		count, err := strconv.ParseInt(p.Labels[RequiredNPUCount], 10, 64)
		if err != nil {
			continue
		}
		occupied += count
	}
	return occupied
}

func getEligibleVirtualNodes(nodeInfos []*framework.NodeInfo, nsOffloading *unstructured.Unstructured) map[string]bool {
	result := make(map[string]bool)
	if nsOffloading == nil {
		return result
	}
	terms, hasTerms := extractClusterSelectorTerms(nsOffloading)
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !isVirtualNode(node) {
			continue
		}
		if !hasTerms || nodeSelectorTermsMatch(labels.Set(node.Labels), terms) {
			result[node.Name] = true
		}
	}
	return result
}

// extractClusterSelectorTerms parses the spec.clusterSelector field of a
// NamespaceOffloading object. The clusterSelector is a v1.NodeSelector (with
// nodeSelectorTerms), NOT a metav1.LabelSelector. Parsing it as a
// LabelSelector silently drops nodeSelectorTerms and yields an empty selector
// that matches everything, which would incorrectly admit non-targeted remote
// clusters (e.g. gy005 when only gy004 is selected).
func extractClusterSelectorTerms(obj *unstructured.Unstructured) ([]v1.NodeSelectorTerm, bool) {
	selectorMap, found, err := unstructured.NestedMap(obj.Object, "spec", "clusterSelector")
	if !found || err != nil {
		return nil, false
	}
	bytes, err := json.Marshal(selectorMap)
	if err != nil {
		return nil, false
	}
	var ns v1.NodeSelector
	if err := json.Unmarshal(bytes, &ns); err != nil {
		return nil, false
	}
	if len(ns.NodeSelectorTerms) == 0 {
		return nil, false
	}
	return ns.NodeSelectorTerms, true
}

// nodeSelectorTermsMatch evaluates nodeSelectorTerms against node labels.
// Terms are ORed; within a term, requirements are ANDed. This mirrors the
// standard Kubernetes NodeSelector semantics. matchFields are not evaluated
// (label-only matching); a term containing matchFields is skipped to avoid
// falsely admitting a node that should be constrained by a field requirement.
func nodeSelectorTermsMatch(nodeLabels labels.Set, terms []v1.NodeSelectorTerm) bool {
	for _, term := range terms {
		if nodeSelectorTermMatch(nodeLabels, term) {
			return true
		}
	}
	return false
}

func nodeSelectorTermMatch(nodeLabels labels.Set, term v1.NodeSelectorTerm) bool {
	if len(term.MatchFields) > 0 {
		return false
	}
	if len(term.MatchExpressions) == 0 {
		return false
	}
	for _, req := range term.MatchExpressions {
		if !nodeSelectorRequirementMatch(nodeLabels, req) {
			return false
		}
	}
	return true
}

func nodeSelectorRequirementMatch(nodeLabels labels.Set, req v1.NodeSelectorRequirement) bool {
	switch req.Operator {
	case v1.NodeSelectorOpIn:
		if !nodeLabels.Has(req.Key) {
			return false
		}
		val := nodeLabels.Get(req.Key)
		for _, v := range req.Values {
			if val == v {
				return true
			}
		}
		return false
	case v1.NodeSelectorOpNotIn:
		if !nodeLabels.Has(req.Key) {
			return true
		}
		val := nodeLabels.Get(req.Key)
		for _, v := range req.Values {
			if val == v {
				return false
			}
		}
		return true
	case v1.NodeSelectorOpExists:
		return nodeLabels.Has(req.Key)
	case v1.NodeSelectorOpDoesNotExist:
		return !nodeLabels.Has(req.Key)
	case v1.NodeSelectorOpGt, v1.NodeSelectorOpLt:
		if len(req.Values) != 1 || !nodeLabels.Has(req.Key) {
			return false
		}
		labelVal, err := strconv.ParseInt(nodeLabels.Get(req.Key), 10, 64)
		if err != nil {
			return false
		}
		compareVal, err := strconv.ParseInt(req.Values[0], 10, 64)
		if err != nil {
			return false
		}
		if req.Operator == v1.NodeSelectorOpGt {
			return labelVal > compareVal
		}
		return labelVal < compareVal
	default:
		return false
	}
}
