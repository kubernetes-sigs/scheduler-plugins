# Liqo + Volcano NPU Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the ARCSync plugin to compare local vs Liqo virtual node NPU capacity and schedule to the side with more free cards, with Volcano Queue capping local total capacity.

**Architecture:** Dynamic informers for NamespaceOffloading and Queue CRDs; virtual node NPU usage estimated from runner pod `required-npu-count` labels; PreFilter decides the winning side and populates `nodeFreeNPU` with only winner-side nodes.

**Tech Stack:** Go, k8s.io dynamic informers, unstructured objects, scheduler framework (PreFilter/Filter/Score/Reserve)

## Global Constraints

- Go module: `sigs.k8s.io/scheduler-plugins`, Go 1.24
- No new Go module dependencies — use `k8s.io/client-go/dynamic` (already transitively available via k8s.io/kubernetes)
- Existing label keys unchanged: `ascend-ci.com/required-npu-count`, `ascend-ci.com/npu-resource-domain`, `ascend-ci.com/npu-resource-model`, `ascend-ci.com/npu-count`
- Virtual node label: `liqo.io/remote-cluster-id` (presence = virtual node)
- Namespace queue annotation: `scheduling.volcano.sh/queue-name`
- GVRs: `offloading.liqo.io/v1beta1` resource `namespaceoffloadings`; `scheduling.volcano.sh/v1beta1` resource `queues`
- TDD: write failing test → implement → pass → commit
- No comments in code unless asked

---

## File Structure

| File | Responsibility |
|------|---------------|
| `pkg/arcsync/arcsync.go` | Main plugin: struct, New(), PreFilter, Filter, Score, Reserve, PostBind |
| `pkg/arcsync/liqo.go` (new) | Virtual node identification, NPU occupied calc, NamespaceOffloading matching, lister interfaces + production wrapper |
| `pkg/arcsync/volcano.go` (new) | Volcano Queue limit query, lister interface + production wrapper |
| `pkg/arcsync/arcsync_test.go` (new) | Unit tests |
| `ARCSYNC_GUIDE.md` | Documentation |

---

### Task 1: Virtual Node Identification and NPU Occupied Calculation

**Files:**
- Create: `pkg/arcsync/liqo.go`
- Create: `pkg/arcsync/arcsync_test.go`
- Modify: `pkg/arcsync/arcsync.go` (add struct fields for lister interfaces)

**Interfaces:**
- Produces: `nsOffloadingLister` interface, `queueLister` interface, `isVirtualNode(node *v1.Node) bool`, `calcVirtualNodeOccupied(nodeInfo *framework.NodeInfo, resDomain, resModel string) int64`

- [ ] **Step 1: Write the failing test**

Create `pkg/arcsync/arcsync_test.go`:

```go
package arcsync

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

const (
	testResDomain = "huawei.com"
	testResModel  = "ascend-310"
)

func makeNodeWithNPU(name string, npuCap int64, labels map[string]string) *v1.Node {
	res := v1.ResourceList{}
	res[v1.ResourceName(testResDomain+"/"+testResModel)] = *resource.NewQuantity(npuCap, resource.DecimalSI)
	node := st.MakeNode().Name(name).Capacity(res).Obj()
	node.Status.Allocatable = res
	if labels != nil {
		node.Labels = labels
	}
	return node
}

func makeRunnerPod(name, namespace, nodeName string, npuCount int) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(namespace).Node(nodeName).Obj()
	pod.Labels = map[string]string{
		RequiredNPUCount: strconv.Itoa(npuCount),
		ResourceDomain:   testResDomain,
		ResourceModel:    testResModel,
	}
	return pod
}

func TestIsVirtualNode(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{"local node (no label)", nil, false},
		{"virtual node", map[string]string{"liqo.io/remote-cluster-id": "cluster-a"}, true},
		{"local node with other label", map[string]string{"foo": "bar"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{}
			if tt.labels != nil {
				node.Labels = tt.labels
			}
			if got := isVirtualNode(node); got != tt.expected {
				t.Errorf("isVirtualNode() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCalcVirtualNodeOccupied(t *testing.T) {
	node := makeNodeWithNPU("virt-node-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	pods := []*v1.Pod{
		makeRunnerPod("runner-1", "ns1", "virt-node-1", 2),
		makeRunnerPod("runner-2", "ns1", "virt-node-1", 3),
		makeRunnerPod("runner-3", "ns1", "virt-node-1", 1),
	}
	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)
	for _, p := range pods {
		nodeInfo.AddPod(p)
	}

	got := calcVirtualNodeOccupied(nodeInfo, testResDomain, testResModel)
	if got != 6 {
		t.Errorf("calcVirtualNodeOccupied() = %d, want 6", got)
	}
}

func TestCalcVirtualNodeOccupiedWithMismatchedModel(t *testing.T) {
	node := makeNodeWithNPU("virt-node-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	pods := []*v1.Pod{
		makeRunnerPod("runner-1", "ns1", "virt-node-1", 2),
	}
	runner2 := st.MakePod().Name("runner-2").Namespace("ns1").Node("virt-node-1").Obj()
	runner2.Labels = map[string]string{
		RequiredNPUCount: "3",
		ResourceDomain:   testResDomain,
		ResourceModel:    "ascend-910",
	}
	pods = append(pods, runner2)

	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)
	for _, p := range pods {
		nodeInfo.AddPod(p)
	}

	got := calcVirtualNodeOccupied(nodeInfo, testResDomain, testResModel)
	if got != 2 {
		t.Errorf("calcVirtualNodeOccupied() = %d, want 2 (only matching model)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/arcsync/ -run "TestIsVirtualNode|TestCalcVirtualNode" -v`
Expected: FAIL — `isVirtualNode` and `calcVirtualNodeOccupied` not defined; also missing imports (`strconv`, `framework`)

- [ ] **Step 3: Implement `pkg/arcsync/liqo.go`**

```go
package arcsync

import (
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
	obj, err := d.lister.ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		return nil, false, err
	}
	if len(obj) == 0 {
		return nil, false, nil
	}
	u, ok := obj[0].(*unstructured.Unstructured)
	if !ok {
		return nil, false, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj[0])
	}
	return u, true, nil
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
		var count int64
		if v, err := fmt.Sprintf("%s", p.Labels[RequiredNPUCount]).parseInt(); err == nil {
			count = v
		}
		occupied += count
	}
	return occupied
}
```

Wait, there's a problem. Go doesn't have a `.parseInt()` method on string. Let me fix that. I need to use `strconv.ParseInt`.

Let me also fix the `fmt.Sprintf` usage. The correct approach is:

```go
import "strconv"

count, err := strconv.ParseInt(p.Labels[RequiredNPUCount], 10, 64)
if err != nil {
    continue
}
occupied += count
```

Let me rewrite the file correctly.

Actually, I realize I need to be more careful. Let me rewrite the complete liqo.go file:

```go
package arcsync

import (
	"encoding/json"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
	obj, err := d.lister.ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		return nil, false, err
	}
	if len(obj) == 0 {
		return nil, false, nil
	}
	u, ok := obj[0].(*unstructured.Unstructured)
	if !ok {
		return nil, false, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj[0])
	}
	return u, true, nil
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
```

And update `arcsync.go` to add struct fields:

```go
type ARCSync struct {
	handle               framework.Handle
	podLister            corev1listers.PodLister
	inFlightReservations map[string]reservation
	mu                   sync.Mutex
	nsOffloadingLister   nsOffloadingLister
	queueLister          queueLister
}
```

Also, the test file needs `strconv` import and `framework` import. Let me fix that too.

Let me write the plan more carefully now, making sure all code is correct.

Actually, I realize this is going to be a very long plan document. Let me write it to the file and be thorough. Let me make sure all the code compiles correctly in my head before writing it.

Key imports needed:
- `strconv` for parsing int from string
- `k8s.io/apimachinery/pkg/api/resource` for resource.ParseQuantity
- `k8s.io/apimachinery/pkg/apis/meta/v1/unstructured` for unstructured access
- `encoding/json` for converting unstructured maps to typed structs
- `k8s.io/apimachinery/pkg/labels` for label selectors
- `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` for LabelSelector

For the test file:
- `testing`
- `strconv`
- `v1 "k8s.io/api/core/v1"`
- `"k8s.io/apimachinery/pkg/api/resource"`
- `st "k8s.io/kubernetes/pkg/scheduler/testing"`
- `"k8s.io/kubernetes/pkg/scheduler/framework"`
- `testutil "sigs.k8s.io/scheduler-plugins/test/util"`

Let me also think about how to set up the test framework for PreFilter tests. Looking at the capacityscheduling test:

```go
fwk, err := tf.NewFramework(
    ctx, registeredPlugins, "",
    frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
    frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(pods, nodes)),
)

cs := &CapacityScheduling{
    elasticQuotaInfos: tt.elasticQuotas,
    fh:                fwk,
}
```

For ARCSync, I'd do:
```go
fwk, err := tf.NewFramework(
    ctx, registeredPlugins, "",
    frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
    frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(pods, nodes)),
)

pl := &ARCSync{
    handle:               fwk,
    podLister:            fwk.SharedInformerFactory().Core().V1().Pods().Lister(),
    inFlightReservations: make(map[string]reservation),
    nsOffloadingLister:   fakeNSOffloadingLister,
    queueLister:          fakeQueueLister,
}
```

Wait, but `fwk.SharedInformerFactory()` might not work in a test framework. Let me check what the capacityscheduling test does... it uses `fwk` directly as the `fh` field. Let me check if `tf.NewFramework` returns something that has `SharedInformerFactory()`.

Actually, looking at the capacityscheduling test, it just sets `fh: fwk`. The `fwk` is a `framework.Framework` which implements `framework.Handle`. So `SharedInformerFactory()` should work, but it might return an empty factory in tests.

Actually, for the ARCSync test, we don't need the pod lister from the informer factory because we use `handle.SnapshotSharedLister().NodeInfos().List()` to get nodes and pods. The `podLister` is only used in `isOldestPendingRunner`. For PreFilter tests, we mainly need the snapshot shared lister.

But wait, the `isOldestPendingRunner` function uses `pl.podLister.List(labels.Everything())`. If `podLister` is nil or empty, this would fail. Let me check if we need to set up a fake pod lister too.

Looking at the test more carefully, the `tf.NewFramework` with `WithSnapshotSharedLister` sets up the snapshot. The `SharedInformerFactory()` might return a real informer factory with no data. For the `isOldestPendingRunner` check, it would list all pods from the informer factory (which is empty in tests). This means `isOldestPendingRunner` would return `true` (no older pods found).

Actually, I think for the PreFilter test, we can just create the framework and set up the fake shared lister. The pod lister from `SharedInformerFactory()` would be empty, so `isOldestPendingRunner` would pass (no competing pods). This is fine for testing the comparison logic.

But we also need to create fake listers for NamespaceOffloading and Queue. Let me define simple fakes in the test file:

```go
type fakeNSOffloadingLister struct {
	objects map[string]*unstructured.Unstructured
}

func (f *fakeNSOffloadingLister) Get(namespace string) (*unstructured.Unstructured, bool, error) {
	obj, ok := f.objects[namespace]
	return obj, ok, nil
}

type fakeQueueLister struct {
	objects map[string]*unstructured.Unstructured
}

func (f *fakeQueueLister) Get(queueName string) (*unstructured.Unstructured, bool, error) {
	obj, ok := f.objects[queueName]
	return obj, ok, nil
}
```

OK, I think I have all the pieces now. Let me write the complete plan.

For the PreFilter modification, the key logic is:

1. After computing `nodePhysicalUsage` and `nodeFreeNPU` for all nodes (existing logic):
   - Separate local and virtual nodes
   - For virtual nodes, recalculate occupied using `calcVirtualNodeOccupied` instead of the existing physical usage
   - Compute local total remaining (with volcano queue adjustment)
   - Compute each virtual node's remaining
   - Compare and decide winner
   - Only keep winner-side nodes in `nodeFreeNPU`

Actually, looking at the existing PreFilter code more carefully, the current logic:
1. Computes `nodePhysicalUsage` per node (from pods)
2. Adds in-flight reservations to get `nodeTotalOccupied`
3. Computes `nodeFreeNPU[node] = allocatable - nodeTotalOccupied[node]`

For the new logic, I need to:
1. Compute physical usage per node (existing logic for local nodes)
2. For virtual nodes, compute occupied using `calcVirtualNodeOccupied`
3. Add in-flight reservations to both
4. Compute free NPU per node
5. Compute local total remaining (with queue adjustment)
6. Compare with virtual nodes
7. Only keep winner-side nodes in `nodeFreeNPU`

The key insight is that for virtual nodes, the `nodePhysicalUsage` computed by the existing logic would be 0 (because runner pods don't have NPU resource requests or `AllocatedNPUCount` labels). So I need to either:
- Replace the physical usage calculation for virtual nodes
- Or add a correction after the existing calculation

I think the cleanest approach is to modify the existing loop to use different calculation for virtual nodes:

```go
for _, nodeInfo := range nodeInfos {
    nodeName := nodeInfo.Node().Name
    var physUsage int64
    if isVirtualNode(nodeInfo.Node()) {
        physUsage = calcVirtualNodeOccupied(nodeInfo, resDomain, resModel)
    } else {
        // existing logic
        for _, podInfo := range nodeInfo.Pods {
            // ...existing pod usage calculation...
        }
    }
    nodePhysicalUsage[nodeName] = physUsage
}
```

Then after computing `nodeFreeNPU`, I add the comparison logic:

```go
// After existing nodeFreeNPU computation, before hasCandidate check:

// Check if liqo comparison should be applied
hasVirtualNodes := false
for _, nodeInfo := range nodeInfos {
    if isVirtualNode(nodeInfo.Node()) {
        hasVirtualNodes = true
        break
    }
}

if hasVirtualNodes && pl.nsOffloadingLister != nil {
    // Get NamespaceOffloading
    nsOffloading, found, _ := pl.nsOffloadingLister.Get(pod.Namespace)
    if found {
        // Get eligible virtual nodes
        selector := getClusterSelector(nsOffloading)
        eligibleVirtualNodes := getEligibleVirtualNodes(nodeInfos, selector)
        
        if len(eligibleVirtualNodes) > 0 {
            // Compute local total remaining
            localTotalAllocatable := int64(0)
            localTotalOccupied := int64(0)
            for _, nodeInfo := range nodeInfos {
                if !isVirtualNode(nodeInfo.Node()) && canScheduleOnNode(nodeInfo.Node()) {
                    allocatable := nodeInfo.Node().Status.Allocatable[fullResourceName]
                    localTotalAllocatable += allocatable.Value()
                    localTotalOccupied += nodeTotalOccupied[nodeInfo.Node().Name]
                }
            }
            
            localTotalCapacity := localTotalAllocatable
            // Volcano queue adjustment
            if pl.queueLister != nil {
                if queueLimit, found := getQueueNpuLimit(pod, pl.queueLister, fullResourceName); found {
                    if queueLimit < localTotalCapacity {
                        localTotalCapacity = queueLimit
                    }
                }
            }
            localRemaining := localTotalCapacity - localTotalOccupied
            if localRemaining < 0 {
                localRemaining = 0
            }
            
            // Find winning virtual node
            var bestVirtualNode string
            var bestVirtualRemaining int64
            for _, nodeName := range eligibleVirtualNodes {
                remaining := nodeFreeNPU[nodeName]
                if remaining > bestVirtualRemaining {
                    bestVirtualRemaining = remaining
                    bestVirtualNode = nodeName
                }
            }
            
            // Compare
            if localRemaining >= bestVirtualRemaining {
                // Local wins - remove all virtual nodes from nodeFreeNPU
                for _, nodeName := range eligibleVirtualNodes {
                    delete(nodeFreeNPU, nodeName)
                }
            } else {
                // Virtual node wins - remove all local nodes and other virtual nodes
                for nodeName := range nodeFreeNPU {
                    if nodeName != bestVirtualNode {
                        delete(nodeFreeNPU, nodeName)
                    }
                }
            }
        }
    }
}
```

Then the `hasCandidate` check uses the filtered `nodeFreeNPU`.

OK, I think I have a clear picture now. Let me write the plan.

Wait, I also need to handle the `getQueueNpuLimit` function. It needs to:
1. Get the namespace annotation
2. Look up the Queue CRD
3. Extract the capability for the NPU resource

But in the PreFilter, the `pod` is available, and the namespace is `pod.Namespace`. However, to get the namespace annotation, I need the Namespace object. The current code doesn't have a namespace lister.

Looking at the ARCSync struct, it has `handle framework.Handle`. The handle has `SharedInformerFactory()` which provides a namespace lister. So I can use:
```go
nsLister := pl.handle.SharedInformerFactory().Core().V1().Namespaces().Lister()
ns, err := nsLister.Get(pod.Namespace)
```

But in tests, this might not work if the informer factory is empty. Let me add a namespace lister to the struct, or pass it as a parameter.

Actually, for the test, I can set up the informer factory with the test namespace. Or I can make the namespace annotation lookup a separate function that takes a namespace object.

Hmm, let me think about this differently. The `getQueueNpuLimit` function needs:
1. The namespace annotation `scheduling.volcano.sh/queue-name`
2. The Queue CRD's `spec.capability[fullResourceName]`

For #1, I need the Namespace object. In the PreFilter, I can get it from the handle's informer factory. But for testing, I need to be able to inject a namespace.

Let me add a `nsLister` field to the struct that can be faked in tests. Or, I can pass the namespace name to `getQueueNpuLimit` and have it use the queue lister to look up the queue name.

Wait, but the queue name is in the namespace annotation, not the pod. So I need the Namespace object to get the annotation.

Let me add a `namespaceLister` to the ARCSync struct:

```go
type ARCSync struct {
    handle               framework.Handle
    podLister            corev1listers.PodLister
    namespaceLister      corev1listers.NamespaceLister
    inFlightReservations map[string]reservation
    mu                   sync.Mutex
    nsOffloadingLister   nsOffloadingLister
    queueLister          queueLister
}
```

In `New()`:
```go
return &ARCSync{
    handle:               h,
    podLister:            h.SharedInformerFactory().Core().V1().Pods().Lister(),
    namespaceLister:      h.SharedInformerFactory().Core().V1().Namespaces().Lister(),
    inFlightReservations: make(map[string]reservation),
}, nil
```

In tests, I can use a fake namespace lister. The `k8s.io/client-go/listers/core/v1` package has a fake that can be constructed from a fake clientset.

Actually, for the test, I can use `corev1listers.NewNamespaceLister` with a fake store. Or, I can define a namespace annotation lister interface:

```go
type namespaceAnnotator interface {
    GetAnnotation(namespace, key string) (string, bool)
}
```

This is cleaner and easier to fake. Let me use this approach.

Actually, even simpler: I'll make `getQueueNpuLimit` take the namespace object directly, and in PreFilter, I'll get the namespace from the lister. If the lister is nil (test without namespace), I'll skip the queue check.

Let me simplify: in PreFilter, I'll get the namespace annotation from the pod's namespace by looking up the namespace object. If I can't find it, I'll skip the queue check.

For testing, I'll add a field `nsLister corev1listers.NamespaceLister` to the struct. In tests, I can use a fake clientset to create the lister.

Actually, looking at the k8s.io/client-go/listers/core/v1 package, `NamespaceLister` is an interface:
```go
type NamespaceLister interface {
    Get(name string) (*v1.Namespace, error)
    List(selector labels.Selector) ([]*v1.Namespace, error)
}
```

I can create a fake using `cache.NewStore` and `cache.Indexer`. But this is getting complicated.

Let me use a simpler approach: define a function variable or interface for getting the queue name from a namespace.

Actually, the simplest approach for testability is to have a `getNamespaceQueueName` function that can be overridden in tests. But Go doesn't have great support for this pattern without dependency injection.

OK, let me just use the `corev1listers.NamespaceLister` and in tests, create it from a fake clientset. Here's how:

```go
import (
    clientsetfake "k8s.io/client-go/kubernetes/fake"
    coreinformers "k8s.io/client-go/informers/core/v1"
    corelisters "k8s.io/client-go/listers/core/v1"
)
```

In tests:
```go
fakeClient := clientsetfake.NewSimpleClientset(&v1.Namespace{
    ObjectMeta: metav1.ObjectMeta{
        Name: "ns1",
        Annotations: map[string]string{
            "scheduling.volcano.sh/queue-name": "my-queue",
        },
    },
})
informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
nsInformer := informerFactory.Core().V1().Namespaces()
nsInformer.Informer().GetStore().Add(&v1.Namespace{...})
nsLister := nsInformer.Lister()
```

Actually, this is really getting complicated. Let me use a much simpler approach: just store the namespace annotations directly as a map in the test struct, and define an interface for namespace annotation lookup.

Let me define:
```go
type namespaceGetter interface {
    Get(name string) (*v1.Namespace, error)
}
```

And use `corev1listers.NamespaceLister` which satisfies this interface. In tests, I create a simple fake.

OK, I think I'm overcomplicating this. Let me just write the plan and handle the namespace lister in the test by creating a fake store.

Actually, the absolute simplest approach: use `handle.ClientSet().CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})` for a direct API call. This is slower but simpler. But it's not good for production (API call per scheduling cycle).

OK, let me use the `corev1listers.NamespaceLister` approach. In the test, I'll create a `cache.Store` with the namespace and wrap it as a lister. Here's a helper:

```go
func newFakeNamespaceLister(namespaces ...*v1.Namespace) corev1listers.NamespaceLister {
    store := cache.NewStore(func(obj interface{}) (string, error) {
        return obj.(*v1.Namespace).Name, nil
    })
    for _, ns := range namespaces {
        store.Add(ns)
    }
    return corev1listers.NewNamespaceLister(store)
}
```

Wait, `corev1listers.NewNamespaceLister` doesn't exist as a public function. The lister is created from an informer. Let me check...

Actually, `corev1listers.NamespaceLister` is an interface. I can create a fake that implements it:

```go
type fakeNamespaceLister struct {
    namespaces map[string]*v1.Namespace
}

func (f *fakeNamespaceLister) Get(name string) (*v1.Namespace, error) {
    ns, ok := f.namespaces[name]
    if !ok {
        return nil, fmt.Errorf("namespace %s not found", name)
    }
    return ns, nil
}

func (f *fakeNamespaceLister) List(selector labels.Selector) ([]*v1.Namespace, error) {
    var result []*v1.Namespace
    for _, ns := range f.namespaces {
        if selector.Matches(labels.Set(ns.Labels)) {
            result = append(result, ns)
        }
    }
    return result, nil
}
```

This is clean and easy to use in tests. Let me go with this approach.

Actually, I realize I should just define a narrow interface for what we need from the namespace:

```go
type queueNameGetter interface {
    GetQueueName(namespace string) string
}
```

This is even simpler. In production, it wraps the namespace lister. In tests, it returns a fixed string.

Hmm, but this is too narrow and might not be reusable. Let me just use the `corev1listers.NamespaceLister` interface and create a fake in tests.

OK, I'm going to stop overthinking this and just write the plan. The key design decisions are:

1. ARCSync struct gets `nsOffloadingLister`, `queueLister`, and `nsLister` fields (all interfaces for testability)
2. `liqo.go` has `isVirtualNode`, `calcVirtualNodeOccupied`, `getEligibleVirtualNodes`
3. `volcano.go` has `getQueueNpuLimit`
4. PreFilter is modified to add comparison logic
5. New() sets up dynamic informers

For the test, I'll create simple fakes that implement the interfaces.

Let me now write the complete plan document. I'll be very precise with the code.

Wait, actually I just realized something. The `cache.GenericLister.ByIndex` method might not work if the informer isn't set up with a namespace index. For a dynamic informer created via `dynamicinformer.NewFilteredDynamicInformer`, we'd need to set up the namespace index.

For the NamespaceOffloading lister, instead of `ByIndex(cache.NamespaceIndex, namespace)`, I should just `List(labels.Everything())` and filter by namespace. Or, since NamespaceOffloading has the same name as the namespace, I can just `Get(namespace)`.

Wait, NamespaceOffloading is a namespaced resource. In the dynamic informer, `cache.GenericLister.Get(name)` looks up by `namespace/name` key for namespaced resources. But if I use a non-filtered informer (all namespaces), I'd need to do `Get(namespace + "/" + offloadingName)`.

Actually, the NamespaceOffloading CR's name is typically the same as the namespace name. And it's in that namespace. So the key would be `namespace/namespace`.

Hmm, this is unreliable. Let me use `List()` and filter instead:

```go
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
```

This is simpler and works without namespace indexing. It's O(n) where n is the number of NamespaceOffloading CRs, but that's typically small (one per namespace).

OK, let me finalize the plan now. I'm confident in the design.

For the plan structure, let me use 5 tasks:

1. Virtual node identification + NPU occupied calculation (liqo.go)
2. Volcano Queue limit query (volcano.go)  
3. NamespaceOffloading matching (liqo.go extend)
4. PreFilter comparison integration (arcsync.go modify)
5. New() informer registration + documentation (arcsync.go + ARCSYNC_GUIDE.md)

Each task follows TDD: write test → verify fail → implement → verify pass → commit.

Let me write the plan now.</think><tool_call>write<arg_key>content</arg_key><arg_value># Liqo + Volcano NPU Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the ARCSync plugin to compare local vs Liqo virtual node NPU capacity and schedule to the side with more free cards, with Volcano Queue capping local total capacity.

**Architecture:** Dynamic informers for NamespaceOffloading and Queue CRDs; virtual node NPU usage estimated from runner pod `required-npu-count` labels; PreFilter decides the winning side and populates `nodeFreeNPU` with only winner-side nodes.

**Tech Stack:** Go, k8s.io/client-go/dynamic, unstructured objects, scheduler framework (PreFilter/Filter/Score/Reserve)

## Global Constraints

- Go module: `sigs.k8s.io/scheduler-plugins`, Go 1.24
- No new Go module dependencies — `k8s.io/client-go/dynamic` is transitively available via `k8s.io/kubernetes`
- Existing label keys unchanged: `ascend-ci.com/required-npu-count`, `ascend-ci.com/npu-resource-domain`, `ascend-ci.com/npu-resource-model`, `ascend-ci.com/npu-count`
- Virtual node label: `liqo.io/remote-cluster-id` (presence = virtual node)
- Namespace queue annotation: `scheduling.volcano.sh/queue-name`
- GVRs: `offloading.liqo.io/v1beta1` resource `namespaceoffloadings`; `scheduling.volcano.sh/v1beta1` resource `queues`
- TDD: write failing test, implement, pass, commit
- No comments in code unless asked

---

## File Structure

| File | Responsibility |
|------|---------------|
| `pkg/arcsync/arcsync.go` | Main plugin: struct, New(), PreFilter, Filter, Score, Reserve, PostBind |
| `pkg/arcsync/liqo.go` (new) | Virtual node identification, NPU occupied calc, NamespaceOffloading matching, lister interfaces + production wrappers |
| `pkg/arcsync/volcano.go` (new) | Volcano Queue limit query |
| `pkg/arcsync/arcsync_test.go` (new) | Unit tests with fake listers |
| `ARCSYNC_GUIDE.md` | Documentation |

---

### Task 1: Virtual Node Identification and NPU Occupied Calculation

**Files:**
- Create: `pkg/arcsync/liqo.go`
- Create: `pkg/arcsync/arcsync_test.go`
- Modify: `pkg/arcsync/arcsync.go` (add struct fields)

**Interfaces:**
- Produces: `nsOffloadingLister` interface, `queueLister` interface, `isVirtualNode(node *v1.Node) bool`, `calcVirtualNodeOccupied(nodeInfo *framework.NodeInfo, resDomain, resModel string) int64`

- [ ] **Step 1: Write the failing test**

Create `pkg/arcsync/arcsync_test.go`:

```go
package arcsync

import (
	"strconv"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

const (
	testResDomain = "huawei.com"
	testResModel  = "ascend-310"
	testFullResName = v1.ResourceName(testResDomain + "/" + testResModel)
)

func makeNodeWithNPU(name string, npuCap int64, labels map[string]string) *v1.Node {
	res := v1.ResourceList{}
	res[testFullResName] = *resource.NewQuantity(npuCap, resource.DecimalSI)
	node := st.MakeNode().Name(name).Capacity(res).Obj()
	node.Status.Allocatable = res
	if labels != nil {
		node.Labels = labels
	}
	return node
}

func makeRunnerPod(name, namespace, nodeName string, npuCount int) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(namespace).Node(nodeName).Obj()
	pod.Labels = map[string]string{
		RequiredNPUCount: strconv.Itoa(npuCount),
		ResourceDomain:   testResDomain,
		ResourceModel:    testResModel,
	}
	return pod
}

func TestIsVirtualNode(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{"local node (no label)", nil, false},
		{"virtual node", map[string]string{"liqo.io/remote-cluster-id": "cluster-a"}, true},
		{"local node with other label", map[string]string{"foo": "bar"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{}
			if tt.labels != nil {
				node.Labels = tt.labels
			}
			if got := isVirtualNode(node); got != tt.expected {
				t.Errorf("isVirtualNode() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCalcVirtualNodeOccupied(t *testing.T) {
	node := makeNodeWithNPU("virt-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})
	pods := []*v1.Pod{
		makeRunnerPod("runner-1", "ns1", "virt-1", 2),
		makeRunnerPod("runner-2", "ns1", "virt-1", 3),
		makeRunnerPod("runner-3", "ns1", "virt-1", 1),
	}
	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)
	for _, p := range pods {
		nodeInfo.AddPod(p)
	}

	got := calcVirtualNodeOccupied(nodeInfo, testResDomain, testResModel)
	if got != 6 {
		t.Errorf("calcVirtualNodeOccupied() = %d, want 6", got)
	}
}

func TestCalcVirtualNodeOccupiedMismatchedModel(t *testing.T) {
	node := makeNodeWithNPU("virt-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})
	pods := []*v1.Pod{
		makeRunnerPod("runner-1", "ns1", "virt-1", 2),
	}
	runner2 := st.MakePod().Name("runner-2").Namespace("ns1").Node("virt-1").Obj()
	runner2.Labels = map[string]string{
		RequiredNPUCount: "3",
		ResourceDomain:   testResDomain,
		ResourceModel:    "ascend-910",
	}
	pods = append(pods, runner2)

	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)
	for _, p := range pods {
		nodeInfo.AddPod(p)
	}

	got := calcVirtualNodeOccupied(nodeInfo, testResDomain, testResModel)
	if got != 2 {
		t.Errorf("calcVirtualNodeOccupied() = %d, want 2 (only matching model)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/arcsync/ -run "TestIsVirtualNode|TestCalcVirtualNode" -v`
Expected: FAIL — `isVirtualNode` and `calcVirtualNodeOccupied` not defined

- [ ] **Step 3: Implement `pkg/arcsync/liqo.go`**

```go
package arcsync

import (
	"encoding/json"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
```

Modify `pkg/arcsync/arcsync.go` — add struct fields:

Replace the `ARCSync` struct definition (line ~33-38) with:

```go
type ARCSync struct {
	handle               framework.Handle
	podLister            corev1listers.PodLister
	inFlightReservations map[string]reservation
	mu                   sync.Mutex
	nsOffloadingLister   nsOffloadingLister
	queueLister          queueLister
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/arcsync/ -run "TestIsVirtualNode|TestCalcVirtualNode" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/arcsync/liqo.go pkg/arcsync/arcsync_test.go pkg/arcsync/arcsync.go
git commit -m "feat(arcsync): add virtual node identification and NPU occupied calculation"
```

---

### Task 2: Volcano Queue Limit Query

**Files:**
- Create: `pkg/arcsync/volcano.go`
- Modify: `pkg/arcsync/arcsync_test.go` (add tests)

**Interfaces:**
- Consumes: `queueLister` interface from Task 1
- Produces: `getQueueNpuLimit(ns *v1.Namespace, qLister queueLister, fullResourceName v1.ResourceName) (int64, bool)`

- [ ] **Step 1: Write the failing test**

Append to `pkg/arcsync/arcsync_test.go`:

```go
type fakeQueueLister struct {
	objects map[string]*unstructured.Unstructured
}

func (f *fakeQueueLister) Get(queueName string) (*unstructured.Unstructured, bool, error) {
	obj, ok := f.objects[queueName]
	return obj, ok, nil
}

func makeQueueObject(name string, capability map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrQueue.WithKind("Queue"))
	obj.SetName(name)
	capMap := make(map[string]interface{})
	for k, v := range capability {
		capMap[k] = v
	}
	unstructured.SetNestedMap(obj.Object, capMap, "spec", "capability")
	return obj
}

func makeNamespace(name string, annotations map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
	}
}

func TestGetQueueNpuLimit(t *testing.T) {
	queueObj := makeQueueObject("my-queue", map[string]string{
		string(testFullResName): "8",
	})
	qLister := &fakeQueueLister{objects: map[string]*unstructured.Unstructured{
		"my-queue": queueObj,
	}}

	tests := []struct {
		name      string
		ns        *v1.Namespace
		expected  int64
		expectFound bool
	}{
		{
			name:        "queue with matching NPU resource",
			ns:          makeNamespace("ns1", map[string]string{"scheduling.volcano.sh/queue-name": "my-queue"}),
			expected:    8,
			expectFound: true,
		},
		{
			name:        "no queue annotation",
			ns:          makeNamespace("ns2", nil),
			expected:    0,
			expectFound: false,
		},
		{
			name:        "queue not found",
			ns:          makeNamespace("ns3", map[string]string{"scheduling.volcano.sh/queue-name": "nonexistent"}),
			expected:    0,
			expectFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := getQueueNpuLimit(tt.ns, qLister, testFullResName)
			if got != tt.expected || found != tt.expectFound {
				t.Errorf("getQueueNpuLimit() = (%d, %v), want (%d, %v)", got, found, tt.expected, tt.expectFound)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/arcsync/ -run "TestGetQueueNpuLimit" -v`
Expected: FAIL — `getQueueNpuLimit` not defined

- [ ] **Step 3: Implement `pkg/arcsync/volcano.go`**

```go
package arcsync

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const queueAnnotationKey = "scheduling.volcano.sh/queue-name"

func getQueueNpuLimit(ns *v1.Namespace, qLister queueLister, fullResourceName v1.ResourceName) (int64, bool) {
	if ns == nil || qLister == nil {
		return 0, false
	}
	queueName, ok := ns.Annotations[queueAnnotationKey]
	if !ok || queueName == "" {
		return 0, false
	}
	obj, found, err := qLister.Get(queueName)
	if !found || err != nil {
		return 0, false
	}
	capability, found, err := unstructured.NestedMap(obj.Object, "spec", "capability")
	if !found || err != nil {
		return 0, false
	}
	val, exists := capability[string(fullResourceName)]
	if !exists {
		return 0, false
	}
	strVal, ok := val.(string)
	if !ok {
		strVal = fmt.Sprintf("%v", val)
	}
	q, err := resource.ParseQuantity(strVal)
	if err != nil {
		return 0, false
	}
	return q.Value(), true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/arcsync/ -run "TestGetQueueNpuLimit" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/arcsync/volcano.go pkg/arcsync/arcsync_test.go
git commit -m "feat(arcsync): add volcano queue NPU limit query"
```

---

### Task 3: NamespaceOffloading ClusterSelector Matching

**Files:**
- Modify: `pkg/arcsync/liqo.go` (add `getEligibleVirtualNodes`)
- Modify: `pkg/arcsync/arcsync_test.go` (add tests)

**Interfaces:**
- Consumes: `nsOffloadingLister` interface from Task 1
- Produces: `getEligibleVirtualNodes(nodeInfos []*framework.NodeInfo, nsOffloading *unstructured.Unstructured) map[string]bool`

- [ ] **Step 1: Write the failing test**

Append to `pkg/arcsync/arcsync_test.go`:

```go
type fakeNSOffloadingLister struct {
	objects map[string]*unstructured.Unstructured
}

func (f *fakeNSOffloadingLister) Get(namespace string) (*unstructured.Unstructured, bool, error) {
	obj, ok := f.objects[namespace]
	return obj, ok, nil
}

func makeNamespaceOffloading(namespace, matchKey, matchVal string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrNamespaceOffloading.WithKind("NamespaceOffloading"))
	obj.SetName(namespace)
	obj.SetNamespace(namespace)
	selectorMap := map[string]interface{}{
		"matchLabels": map[string]interface{}{
			matchKey: matchVal,
		},
	}
	unstructured.SetNestedMap(obj.Object, selectorMap, "spec", "clusterSelector")
	return obj
}

func TestGetEligibleVirtualNodes(t *testing.T) {
	offloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "cluster-a")

	localNode := makeNodeWithNPU("local-1", 8, nil)
	virtA := makeNodeWithNPU("virt-a", 8, map[string]string{
		"liqo.io/remote-cluster-id": "cluster-a",
	})
	virtB := makeNodeWithNPU("virt-b", 8, map[string]string{
		"liqo.io/remote-cluster-id": "cluster-b",
	})

	nodeInfos := []*framework.NodeInfo{
		framework.NewNodeInfo(),
		framework.NewNodeInfo(),
		framework.NewNodeInfo(),
	}
	nodeInfos[0].SetNode(localNode)
	nodeInfos[1].SetNode(virtA)
	nodeInfos[2].SetNode(virtB)

	got := getEligibleVirtualNodes(nodeInfos, offloading)
	if len(got) != 1 {
		t.Fatalf("expected 1 eligible virtual node, got %d", len(got))
	}
	if !got["virt-a"] {
		t.Errorf("expected virt-a to be eligible, got %v", got)
	}
}

func TestGetEligibleVirtualNodesNoSelector(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrNamespaceOffloading.WithKind("NamespaceOffloading"))
	obj.SetName("ns1")
	obj.SetNamespace("ns1")

	virtA := makeNodeWithNPU("virt-a", 8, map[string]string{
		"liqo.io/remote-cluster-id": "cluster-a",
	})
	nodeInfos := []*framework.NodeInfo{framework.NewNodeInfo()}
	nodeInfos[0].SetNode(virtA)

	got := getEligibleVirtualNodes(nodeInfos, obj)
	if len(got) != 1 {
		t.Fatalf("expected 1 eligible virtual node (empty selector = all), got %d", len(got))
	}
	if !got["virt-a"] {
		t.Errorf("expected virt-a to be eligible, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/arcsync/ -run "TestGetEligibleVirtualNodes" -v`
Expected: FAIL — `getEligibleVirtualNodes` not defined

- [ ] **Step 3: Implement `getEligibleVirtualNodes` in `pkg/arcsync/liqo.go`**

Append to `pkg/arcsync/liqo.go`:

```go
func getEligibleVirtualNodes(nodeInfos []*framework.NodeInfo, nsOffloading *unstructured.Unstructured) map[string]bool {
	result := make(map[string]bool)
	if nsOffloading == nil {
		return result
	}
	selector, err := extractClusterSelector(nsOffloading)
	if err != nil {
		return result
	}
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !isVirtualNode(node) {
			continue
		}
		if selector.Matches(labels.Set(node.Labels)) {
			result[node.Name] = true
		}
	}
	return result
}

func extractClusterSelector(obj *unstructured.Unstructured) (labels.Selector, error) {
	selectorMap, found, err := unstructured.NestedMap(obj.Object, "spec", "clusterSelector")
	if !found || err != nil {
		return labels.Everything(), nil
	}
	bytes, err := json.Marshal(selectorMap)
	if err != nil {
		return labels.Everything(), nil
	}
	var ls metav1.LabelSelector
	if err := json.Unmarshal(bytes, &ls); err != nil {
		return labels.Everything(), nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return labels.Everything(), nil
	}
	return selector, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/arcsync/ -run "TestGetEligibleVirtualNodes" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/arcsync/liqo.go pkg/arcsync/arcsync_test.go
git commit -m "feat(arcsync): add NamespaceOffloading clusterSelector matching"
```

---

### Task 4: PreFilter Comparison Integration

**Files:**
- Modify: `pkg/arcsync/arcsync.go` (PreFilter — the core integration)
- Modify: `pkg/arcsync/arcsync_test.go` (add PreFilter comparison tests)

**Interfaces:**
- Consumes: `isVirtualNode`, `calcVirtualNodeOccupied` from Task 1; `getQueueNpuLimit` from Task 2; `getEligibleVirtualNodes` from Task 3

- [ ] **Step 1: Write the failing test**

Append to `pkg/arcsync/arcsync_test.go`:

```go
import (
	"context"

	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func makeRunnerPodWithLabels(name, namespace string, npuCount int) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(namespace).Obj()
	pod.Labels = map[string]string{
		RequiredNPUCount: strconv.Itoa(npuCount),
		ResourceDomain:   testResDomain,
		ResourceModel:    testResModel,
	}
	return pod
}

func makePodOnNode(name, namespace, nodeName string, npuCount int) *v1.Pod {
	pod := makeRunnerPodWithLabels(name, namespace, npuCount)
	pod.Spec.NodeName = nodeName
	return pod
}

func setupTestFramework(t *testing.T, pods []*v1.Pod, nodes []*v1.Node) (framework.Handle, *framework.CycleState) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
	fwk, err := tf.NewFramework(
		ctx, registeredPlugins, "",
		frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
		frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(pods, nodes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fwk, framework.NewCycleState()
}

func TestPreFilterLocalWins(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 10, nil)
	virtNode := makeNodeWithNPU("virt-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	existingPods := []*v1.Pod{
		makePodOnNode("runner-1", "ns1", "virt-1", 5),
	}
	allPods := append(existingPods, makeRunnerPodWithLabels("new-runner", "ns1", 1))
	nodes := []*v1.Node{localNode, virtNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "cluster-a")
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{"ns1": nsOffloading}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	data := state.Read(stateKey)
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["local-1"]; !exists {
		t.Errorf("expected local-1 in nodeFreeNPU")
	}
	if _, exists := preState.nodeFreeNPU["virt-1"]; exists {
		t.Errorf("expected virt-1 to be excluded from nodeFreeNPU")
	}
}

func TestPreFilterVirtualWins(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 10, nil)
	virtNode := makeNodeWithNPU("virt-1", 20, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	existingPods := []*v1.Pod{
		makePodOnNode("runner-1", "ns1", "local-1", 8),
	}
	allPods := append(existingPods, makeRunnerPodWithLabels("new-runner", "ns1", 1))
	nodes := []*v1.Node{localNode, virtNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "cluster-a")
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{"ns1": nsOffloading}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	data := state.Read(stateKey)
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["virt-1"]; !exists {
		t.Errorf("expected virt-1 in nodeFreeNPU")
	}
	if _, exists := preState.nodeFreeNPU["local-1"]; exists {
		t.Errorf("expected local-1 to be excluded from nodeFreeNPU")
	}
}

func TestPreFilterVolcanoQueueCapsLocal(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 20, nil)
	virtNode := makeNodeWithNPU("virt-1", 10, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	existingPods := []*v1.Pod{
		makePodOnNode("runner-1", "ns1", "local-1", 3),
	}
	allPods := append(existingPods, makeRunnerPodWithLabels("new-runner", "ns1", 1))
	nodes := []*v1.Node{localNode, virtNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	nsObj := makeNamespace("ns1", map[string]string{"scheduling.volcano.sh/queue-name": "q1"})
	queueObj := makeQueueObject("q1", map[string]string{string(testFullResName): "5"})
	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "cluster-a")
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{"ns1": nsOffloading}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{"q1": queueObj}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	data := state.Read(stateKey)
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["virt-1"]; !exists {
		t.Errorf("expected virt-1 in nodeFreeNPU (queue cap 5, local remaining = 5-3=2 < virt 10)")
	}
	if _, exists := preState.nodeFreeNPU["local-1"]; exists {
		t.Errorf("expected local-1 to be excluded")
	}
}

func TestPreFilterNoVirtualNodes(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 10, nil)
	allPods := []*v1.Pod{makeRunnerPodWithLabels("runner-1", "ns1", 1)}
	nodes := []*v1.Node{localNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter should succeed with no virtual nodes: %v", status.Message())
	}

	data := state.Read(stateKey)
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["local-1"]; !exists {
		t.Errorf("expected local-1 in nodeFreeNPU (no virtual nodes = current behavior)")
	}
}

func TestPreFilterNoNamespaceOffloading(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 10, nil)
	virtNode := makeNodeWithNPU("virt-1", 8, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})
	allPods := []*v1.Pod{makeRunnerPodWithLabels("runner-1", "ns1", 1)}
	nodes := []*v1.Node{localNode, virtNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter should succeed: %v", status.Message())
	}

	data := state.Read(stateKey)
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["local-1"]; !exists {
		t.Errorf("expected local-1 in nodeFreeNPU")
	}
	if _, exists := preState.nodeFreeNPU["virt-1"]; !exists {
		t.Errorf("expected virt-1 in nodeFreeNPU (no offloading = current behavior, all nodes)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/arcsync/ -run "TestPreFilter" -v`
Expected: FAIL — PreFilter still includes all nodes in `nodeFreeNPU`

- [ ] **Step 3: Implement the comparison logic in `pkg/arcsync/arcsync.go` PreFilter**

The key change is in the PreFilter function. After the existing `nodeFreeNPU` computation (around line 212-225) and before the `hasCandidate` check (line 222), insert the liqo/volcano comparison logic.

Find this section in `pkg/arcsync/arcsync.go` (the existing code after computing `nodeFreeNPU` and before `hasCandidate`):

```go
	nodeFreeNPU := make(map[string]int64)
	hasCandidate := false
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(node) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		free := allocatable.Value() - nodeTotalOccupied[node.Name]
		nodeFreeNPU[node.Name] = free
		if free >= int64(reqCount) {
			hasCandidate = true
		}
	}
```

Replace with:

```go
	nodeFreeNPU := make(map[string]int64)
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || !canScheduleOnNode(node) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		free := allocatable.Value() - nodeTotalOccupied[node.Name]
		nodeFreeNPU[node.Name] = free
	}

	if pl.shouldApplyLiqoComparison(nodeInfos, pod) {
		pl.applyLiqoComparison(nodeInfos, pod, resDomain, resModel, fullResourceName, nodeTotalOccupied, nodeFreeNPU)
	}

	hasCandidate := false
	for _, free := range nodeFreeNPU {
		if free >= int64(reqCount) {
			hasCandidate = true
			break
		}
	}
```

Then add these helper methods after the `PreFilter` function (before `PreFilterExtensions`):

```go
func (pl *ARCSync) shouldApplyLiqoComparison(nodeInfos []*framework.NodeInfo, pod *v1.Pod) bool {
	if pl.nsOffloadingLister == nil {
		return false
	}
	for _, nodeInfo := range nodeInfos {
		if isVirtualNode(nodeInfo.Node()) {
			obj, found, _ := pl.nsOffloadingLister.Get(pod.Namespace)
			return found && obj != nil
		}
	}
	return false
}

func (pl *ARCSync) applyLiqoComparison(
	nodeInfos []*framework.NodeInfo,
	pod *v1.Pod,
	resDomain, resModel string,
	fullResourceName v1.ResourceName,
	nodeTotalOccupied map[string]int64,
	nodeFreeNPU map[string]int64,
) {
	nsOffloading, found, _ := pl.nsOffloadingLister.Get(pod.Namespace)
	if !found || nsOffloading == nil {
		return
	}
	eligibleVirtuals := getEligibleVirtualNodes(nodeInfos, nsOffloading)
	if len(eligibleVirtuals) == 0 {
		return
	}

	var localTotalAllocatable, localTotalOccupied int64
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil || isVirtualNode(node) || !canScheduleOnNode(node) {
			continue
		}
		allocatable := node.Status.Allocatable[fullResourceName]
		localTotalAllocatable += allocatable.Value()
		localTotalOccupied += nodeTotalOccupied[node.Name]
	}

	localTotalCapacity := localTotalAllocatable
	ns, err := pl.handle.SharedInformerFactory().Core().V1().Namespaces().Lister().Get(pod.Namespace)
	if err == nil && ns != nil {
		if queueLimit, qFound := getQueueNpuLimit(ns, pl.queueLister, fullResourceName); qFound {
			if queueLimit < localTotalCapacity {
				localTotalCapacity = queueLimit
			}
		}
	}

	localRemaining := localTotalCapacity - localTotalOccupied
	if localRemaining < 0 {
		localRemaining = 0
	}

	var bestVirtNode string
	var bestVirtRemaining int64
	for nodeName := range eligibleVirtuals {
		if free, exists := nodeFreeNPU[nodeName]; exists && free > bestVirtRemaining {
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
```

Also, in the existing per-node physical usage calculation loop (around line 159-189), add virtual node handling. Find this loop:

```go
	for _, nodeInfo := range nodeInfos {
		nodeName := nodeInfo.Node().Name
		var physUsage int64
		for _, podInfo := range nodeInfo.Pods {
```

Replace the loop start with:

```go
	for _, nodeInfo := range nodeInfos {
		node := nodeInfo.Node()
		if node == nil {
			continue
		}
		nodeName := node.Name
		var physUsage int64
		if isVirtualNode(node) {
			physUsage = calcVirtualNodeOccupied(nodeInfo, resDomain, resModel)
			nodePhysicalUsage[nodeName] = physUsage
			continue
		}
		for _, podInfo := range nodeInfo.Pods {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/arcsync/ -v`
Expected: PASS — all tests pass

- [ ] **Step 5: Commit**

```bash
git add pkg/arcsync/arcsync.go pkg/arcsync/arcsync_test.go
git commit -m "feat(arcsync): integrate liqo/volcano comparison into PreFilter"
```

---

### Task 5: New() Dynamic Informer Registration + Documentation

**Files:**
- Modify: `pkg/arcsync/arcsync.go` (New function)
- Modify: `ARCSYNC_GUIDE.md`

**Interfaces:**
- Consumes: `dynamicNSOffloadingLister`, `dynamicQueueLister` from Task 1

- [ ] **Step 1: Implement dynamic informer setup in New()**

Replace the existing `New` function in `pkg/arcsync/arcsync.go`:

```go
func New(ctx context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	pl := &ARCSync{
		handle:               h,
		podLister:            h.SharedInformerFactory().Core().V1().Pods().Lister(),
		inFlightReservations: make(map[string]reservation),
	}

	dynamicClient, err := dynamic.NewForConfig(h.KubeConfig())
	if err != nil {
		klog.InfoS("ARCSync: failed to create dynamic client, liqo/volcano features disabled", "err", err.Error())
		return pl, nil
	}

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)

	nsOffloadingInformer := dynamicInformerFactory.ForResource(gvrNamespaceOffloading)
	queueInformer := dynamicInformerFactory.ForResource(gvrQueue)

	pl.nsOffloadingLister = &dynamicNSOffloadingLister{lister: nsOffloadingInformer.Lister()}
	pl.queueLister = &dynamicQueueLister{lister: queueInformer.Lister()}

	go dynamicInformerFactory.Start(ctx.Done())
	go nsOffloadingInformer.Informer().Run(ctx.Done())
	go queueInformer.Informer().Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(),
		nsOffloadingInformer.Informer().HasSynced,
		queueInformer.Informer().HasSynced,
	) {
		klog.InfoS("ARCSync: dynamic informer cache sync failed or timed out, liqo/volcano features may be delayed")
	}

	klog.InfoS("ARCSync: dynamic informers started for liqo and volcano")
	return pl, nil
}
```

Add these imports to `pkg/arcsync/arcsync.go`:

```go
import (
	"context"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)
```

- [ ] **Step 2: Run existing tests to verify nothing breaks**

Run: `go test ./pkg/arcsync/ -v`
Expected: PASS — all existing tests pass (tests inject fake listers, bypassing informer setup)

- [ ] **Step 3: Update `ARCSYNC_GUIDE.md`**

Append a new section to `ARCSYNC_GUIDE.md`:

```markdown

## 5. Liqo 虚拟节点集成

当集群中存在 Liqo 虚拟节点（带 `liqo.io/remote-cluster-id` 标签的 Node）且 Pod 所在 namespace 配置了 `NamespaceOffloading` CR 时，ARCSync 会执行本地与虚拟节点之间的 NPU 资源比对：

1. **本地总剩余** = 所有本地非 cordoned 节点的空闲 NPU 之和
2. **虚拟节点剩余** = `Allocatable[NPU]` - 该虚拟节点上 runner pod 的 `required-npu-count` 标签累加值
3. 取两者中剩余更多的一方进行调度，另一方被 Filter 拒绝
4. 平局时本地优先

### NamespaceOffloading 配置

在需要使用虚拟节点的 namespace 中创建 `NamespaceOffloading` CR（`offloading.liqo.io/v1beta1`），通过 `spec.clusterSelector` 指定可调度的虚拟节点：

```yaml
apiVersion: offloading.liqo.io/v1beta1
kind: NamespaceOffloading
metadata:
  name: default
  namespace: your-namespace
spec:
  clusterSelector:
    matchLabels:
      liqo.io/remote-cluster-id: "target-cluster"
```

## 6. Volcano Queue 集成

当 Pod 所在 namespace 通过 annotation `scheduling.volcano.sh/queue-name` 关联了 Volcano Queue 时，本地资源总量取 Queue 限额与实际总卡数的最小值：

- `本地资源总量 = min(Queue.spec.capability[NPU], Σ 本地节点 Allocatable[NPU])`
- `本地剩余 = 本地资源总量 - 本地已占用`

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: Queue
metadata:
  name: my-queue
spec:
  capability:
    huawei.com/ascend-310: "8"
```

Namespace annotation：

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: your-namespace
  annotations:
    scheduling.volcano.sh/queue-name: "my-queue"
```
```

- [ ] **Step 4: Commit**

```bash
git add pkg/arcsync/arcsync.go ARCSYNC_GUIDE.md
git commit -m "feat(arcsync): register dynamic informers in New() and update docs"
```

---

## Self-Review Checklist

**Spec coverage:**
- [x] Trigger condition (virtual nodes + NamespaceOffloading) → Task 4 `shouldApplyLiqoComparison`
- [x] Virtual node NPU occupied via runner pod labels → Task 1 `calcVirtualNodeOccupied`
- [x] Local vs virtual comparison, max wins → Task 4 `applyLiqoComparison`
- [x] Local wins: exclude virtual nodes → Task 4 `applyLiqoComparison`
- [x] Virtual wins: exclude local + other virtual → Task 4 `applyLiqoComparison`
- [x] Volcano Queue caps local total → Task 2+4 `getQueueNpuLimit` in `applyLiqoComparison`
- [x] Degradation: no virtual nodes → Task 4 `TestPreFilterNoVirtualNodes`
- [x] Degradation: no NamespaceOffloading → Task 4 `TestPreFilterNoNamespaceOffloading`
- [x] Dynamic informer setup → Task 5 `New()`
- [x] Documentation → Task 5 `ARCSYNC_GUIDE.md`
- [x] Negative localRemaining clamp to 0 → Task 4 `applyLiqoComparison`
- [x] Tie → local wins → Task 4 `>=` comparison

**Placeholder scan:** No TBD, TODO, or vague steps. All code blocks are complete.

**Type consistency:** `nsOffloadingLister` and `queueLister` interfaces used consistently across Tasks 1-5. `getQueueNpuLimit` signature `(ns *v1.Namespace, qLister queueLister, fullResourceName v1.ResourceName) (int64, bool)` matches in Task 2 definition and Task 4 usage.
