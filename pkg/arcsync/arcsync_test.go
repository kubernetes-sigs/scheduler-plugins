package arcsync

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
)

const (
	testResDomain   = "huawei.com"
	testResModel    = "ascend-310"
	testFullResName = v1.ResourceName(testResDomain + "/" + testResModel)
)

func makeNodeWithNPU(name string, npuCap int64, labels map[string]string) *v1.Node {
	capMap := map[v1.ResourceName]string{testFullResName: strconv.FormatInt(npuCap, 10)}
	node := st.MakeNode().Name(name).Capacity(capMap).Obj()
	node.Status.Allocatable = v1.ResourceList{
		testFullResName: *resource.NewQuantity(npuCap, resource.DecimalSI),
	}
	if labels != nil {
		node.Labels = labels
	}
	return node
}

func makeRunnerPod(name, namespace, nodeName string, npuCount int) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(namespace).Node(nodeName).UID(name).Obj()
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

func TestGetEligibleVirtualNodesExcludesNonTargetedCluster(t *testing.T) {
	offloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "gy004")

	virtGy004 := makeNodeWithNPU("gy004", 224, map[string]string{
		"liqo.io/remote-cluster-id": "gy004",
	})
	virtGy005 := makeNodeWithNPU("gy005", 144, map[string]string{
		"liqo.io/remote-cluster-id": "gy005",
	})

	nodeInfos := []*framework.NodeInfo{
		framework.NewNodeInfo(),
		framework.NewNodeInfo(),
	}
	nodeInfos[0].SetNode(virtGy004)
	nodeInfos[1].SetNode(virtGy005)

	got := getEligibleVirtualNodes(nodeInfos, offloading)
	if len(got) != 1 {
		t.Fatalf("expected 1 eligible virtual node (gy004 only), got %d: %v", len(got), got)
	}
	if !got["gy004"] {
		t.Errorf("expected gy004 to be eligible, got %v", got)
	}
	if got["gy005"] {
		t.Errorf("gy005 must NOT be eligible (not in clusterSelector), but was selected: %v", got)
	}
}

func TestGetEligibleVirtualNodesNoSelector(t *testing.T) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrNamespaceOffloading.GroupVersion().WithKind("NamespaceOffloading"))
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

func makeNamespaceOffloading(namespace, matchKey, matchVal string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrNamespaceOffloading.GroupVersion().WithKind("NamespaceOffloading"))
	obj.SetName(namespace)
	obj.SetNamespace(namespace)
	selectorMap := map[string]interface{}{
		"nodeSelectorTerms": []interface{}{
			map[string]interface{}{
				"matchExpressions": []interface{}{
					map[string]interface{}{
						"key":      matchKey,
						"operator": "In",
						"values":   []interface{}{matchVal},
					},
				},
			},
		},
	}
	unstructured.SetNestedMap(obj.Object, selectorMap, "spec", "clusterSelector")
	return obj
}

func makeQueueObject(name string, capability map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvrQueue.GroupVersion().WithKind("Queue"))
	obj.SetName(name)
	capMap := make(map[string]interface{})
	for k, v := range capability {
		capMap[k] = v
	}
	unstructured.SetNestedMap(obj.Object, capMap, "spec", "capability")
	return obj
}

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

func TestGetQueueNpuLimit(t *testing.T) {
	queueObj := makeQueueObject("my-queue", map[string]string{
		string(testFullResName): "8",
	})
	qLister := &fakeQueueLister{objects: map[string]*unstructured.Unstructured{
		"my-queue": queueObj,
	}}

	tests := []struct {
		name        string
		pod         *v1.Pod
		expected    int64
		expectFound bool
	}{
		{
			name: "pod with queue annotation",
			pod:  st.MakePod().Name("p1").Namespace("ns1").UID("p1").Obj(),
			expected: 8,
			expectFound: true,
		},
		{
			name:        "pod without queue annotation",
			pod:         st.MakePod().Name("p2").Namespace("ns2").UID("p2").Obj(),
			expected:    0,
			expectFound: false,
		},
		{
			name: "queue not found",
			pod: func() *v1.Pod {
				p := st.MakePod().Name("p3").Namespace("ns3").UID("p3").Obj()
				p.Annotations = map[string]string{"scheduling.volcano.sh/queue-name": "nonexistent"}
				return p
			}(),
			expected:    0,
			expectFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "pod with queue annotation" {
				tt.pod.Annotations = map[string]string{"scheduling.volcano.sh/queue-name": "my-queue"}
			}
			got, found := getQueueNpuLimit(tt.pod, qLister, testFullResName)
			if got != tt.expected || found != tt.expectFound {
				t.Errorf("getQueueNpuLimit() = (%d, %v), want (%d, %v)", got, found, tt.expected, tt.expectFound)
			}
		})
	}
}

func makeRunnerPodWithLabels(name, namespace string, npuCount int) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(namespace).UID(name).Obj()
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

type fakeSharedLister struct {
	nodeInfos []*framework.NodeInfo
	nodeMap   map[string]*framework.NodeInfo
}

func newFakeSharedLister(pods []*v1.Pod, nodes []*v1.Node) framework.SharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		if _, ok := nodeInfoMap[node.Name]; !ok {
			nodeInfoMap[node.Name] = framework.NewNodeInfo()
		}
		nodeInfoMap[node.Name].SetNode(node)
	}
	var nodeInfos []*framework.NodeInfo
	for _, ni := range nodeInfoMap {
		nodeInfos = append(nodeInfos, ni)
	}
	return &fakeSharedLister{nodeInfos: nodeInfos, nodeMap: nodeInfoMap}
}

func (f *fakeSharedLister) NodeInfos() framework.NodeInfoLister { return f }
func (f *fakeSharedLister) StorageInfos() framework.StorageInfoLister { return nil }
func (f *fakeSharedLister) List() ([]*framework.NodeInfo, error) { return f.nodeInfos, nil }
func (f *fakeSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) { return nil, nil }
func (f *fakeSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) { return nil, nil }
func (f *fakeSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	ni, ok := f.nodeMap[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeName)
	}
	return ni, nil
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
		frameworkruntime.WithSnapshotSharedLister(newFakeSharedLister(pods, nodes)),
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

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
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

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
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

	queueObj := makeQueueObject("q1", map[string]string{string(testFullResName): "5"})
	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "cluster-a")
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{"ns1": nsOffloading}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{"q1": queueObj}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	targetPod.Annotations = map[string]string{"scheduling.volcano.sh/queue-name": "q1"}
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
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

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
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

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["local-1"]; !exists {
		t.Errorf("expected local-1 in nodeFreeNPU")
	}
	if _, exists := preState.nodeFreeNPU["virt-1"]; exists {
		t.Errorf("expected virt-1 to be EXCLUDED from nodeFreeNPU (no offloading = no virtual nodes)")
	}
}

func TestPreFilterNoOffloadingRejectsWhenOnlyVirtHasNPU(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 2, nil)
	virtNode := makeNodeWithNPU("virt-1", 10, map[string]string{"liqo.io/remote-cluster-id": "cluster-a"})

	wfPod := st.MakePod().Name("wf-1").Namespace("ns1").Node("local-1").UID("wf-1-uid").Obj()
	wfPod.Labels = map[string]string{
		AllocatedNPUCount: "2",
		ResourceDomain:    testResDomain,
		ResourceModel:     testResModel,
	}
	allPods := []*v1.Pod{
		wfPod,
		makeRunnerPodWithLabels("new-runner", "ns1", 2),
	}
	nodes := []*v1.Node{localNode, virtNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 2)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Unschedulable {
		t.Errorf("expected Unschedulable (local-1 full, virt-1 excluded), got %v: %v", status.Code(), status.Message())
	}
}

func TestPreFilterNodeSelectorTargetsVirtual(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 100, nil)
	virtGy005 := makeNodeWithNPU("virt-gy005", 20, map[string]string{"liqo.io/remote-cluster-id": "gy005"})
	virtOther := makeNodeWithNPU("virt-other", 10, map[string]string{"liqo.io/remote-cluster-id": "other"})

	allPods := []*v1.Pod{
		makePodOnNode("runner-1", "ns1", "virt-gy005", 10),
		makeRunnerPodWithLabels("new-runner", "ns1", 1),
	}
	nodes := []*v1.Node{localNode, virtGy005, virtOther}

	fwk, state := setupTestFramework(t, allPods, nodes)

	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "gy005")
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
		nsOffloadingLister:   &fakeNSOffloadingLister{objects: map[string]*unstructured.Unstructured{"ns1": nsOffloading}},
		queueLister:          &fakeQueueLister{objects: map[string]*unstructured.Unstructured{}},
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 1)
	targetPod.Spec.NodeSelector = map[string]string{"liqo.io/remote-cluster-id": "gy005"}
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["virt-gy005"]; !exists {
		t.Errorf("expected virt-gy005 in nodeFreeNPU (pod targets it via nodeSelector)")
	}
	if _, exists := preState.nodeFreeNPU["local-1"]; exists {
		t.Errorf("expected local-1 to be excluded (doesn't match nodeSelector)")
	}
	if _, exists := preState.nodeFreeNPU["virt-other"]; exists {
		t.Errorf("expected virt-other to be excluded (doesn't match nodeSelector)")
	}
}

func TestPreFilterNonEligibleVirtualRemovedWhenLocalWins(t *testing.T) {
	localNode := makeNodeWithNPU("local-1", 100, nil)
	virtGy004 := makeNodeWithNPU("gy004", 8, map[string]string{"liqo.io/remote-cluster-id": "gy004"})
	virtGy005 := makeNodeWithNPU("gy005", 144, map[string]string{"liqo.io/remote-cluster-id": "gy005"})

	allPods := []*v1.Pod{makeRunnerPodWithLabels("new-runner", "ns1", 1)}
	nodes := []*v1.Node{localNode, virtGy004, virtGy005}

	fwk, state := setupTestFramework(t, allPods, nodes)

	nsOffloading := makeNamespaceOffloading("ns1", "liqo.io/remote-cluster-id", "gy004")
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

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	preState := data.(*preFilterState)
	if _, exists := preState.nodeFreeNPU["local-1"]; !exists {
		t.Errorf("expected local-1 in nodeFreeNPU (local wins: localRemaining=100 > gy004 free=8)")
	}
	if _, exists := preState.nodeFreeNPU["gy004"]; exists {
		t.Errorf("expected gy004 to be excluded (local won liqo comparison)")
	}
	if _, exists := preState.nodeFreeNPU["gy005"]; exists {
		t.Errorf("gy005 is NOT targeted by clusterSelector (only gy004) and must be excluded, but leaked into nodeFreeNPU")
	}
}

func TestPreFilterRunnerPodWithArchNodeSelectorSchedules(t *testing.T) {
	// Simulates triton-ascend: runner pod has required-npu-count=4 and
	// nodeSelector arch=amd64. All NPU nodes are arm64. The pod doesn't
	// request NPU in its containers — it only needs NPU reserved somewhere.
	// PreFilter should succeed because NPU capacity exists on arm64 nodes
	// (checked across all nodes, not just nodeSelector-matching ones).
	localNPUNode := makeNodeWithNPU("npu-arm-1", 16, map[string]string{"kubernetes.io/arch": "arm64"})
	localCPUNode := makeNodeWithNPU("cpu-amd-1", 0, map[string]string{"kubernetes.io/arch": "amd64"})
	localCPUNode.Status.Allocatable = v1.ResourceList{}

	allPods := []*v1.Pod{makeRunnerPodWithLabels("new-runner", "ns1", 4)}
	nodes := []*v1.Node{localNPUNode, localCPUNode}

	fwk, state := setupTestFramework(t, allPods, nodes)

	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: make(map[string]reservation),
	}

	targetPod := makeRunnerPodWithLabels("new-runner", "ns1", 4)
	targetPod.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter should succeed (arm64 NPU node has free NPU even though pod targets amd64): %v", status.Message())
	}

	data, err := state.Read(stateKey)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	preState := data.(*preFilterState)
	// arm64 NPU node is NOT in nodeFreeNPU (doesn't match nodeSelector arch=amd64),
	// but hasCandidate checked it separately and found NPU capacity.
	if _, exists := preState.nodeFreeNPU["npu-arm-1"]; exists {
		t.Errorf("npu-arm-1 should NOT be in nodeFreeNPU (doesn't match nodeSelector arch=amd64)")
	}
	// amd64 CPU node IS in nodeFreeNPU (matches nodeSelector), free=0
	if _, exists := preState.nodeFreeNPU["cpu-amd-1"]; !exists {
		t.Errorf("expected cpu-amd-1 in nodeFreeNPU (matches nodeSelector)")
	}
}

func TestFilterRunnerPodPassesCPUNode(t *testing.T) {
	// Runner pod (no NPU request in containers) should pass Filter on a CPU
	// node with free=0, so the default scheduler can place it there.
	localCPUNode := makeNodeWithNPU("cpu-1", 0, nil)
	localCPUNode.Status.Allocatable = v1.ResourceList{}
	localNPUNode := makeNodeWithNPU("npu-1", 16, nil)

	allPods := []*v1.Pod{makeRunnerPodWithLabels("runner", "ns1", 4)}
	nodes := []*v1.Node{localCPUNode, localNPUNode}

	fwk, state := setupTestFramework(t, allPods, nodes)
	pl := &ARCSync{handle: fwk, inFlightReservations: make(map[string]reservation)}

	targetPod := makeRunnerPodWithLabels("runner", "ns1", 4)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Fatalf("PreFilter failed: %v", status.Message())
	}

	// Filter should pass the CPU node (free=0) for runner pods
	ni := framework.NewNodeInfo()
	ni.SetNode(localCPUNode)
	filterStatus := pl.Filter(context.TODO(), state, targetPod, ni)
	if filterStatus.Code() != framework.Success {
		t.Errorf("Filter should pass CPU node for runner pod (no NPU request), got %v: %v", filterStatus.Code(), filterStatus.Message())
	}
}

func TestPreFilterRejectsWhenRunnerReservationsExceedNPU(t *testing.T) {
	// NPU node has 16 NPU. 4 runner pods already reserved 4 each on the NPU
	// node (total 16). The 5th runner must be rejected — per-node hasCandidate
	// sees free = 16 − 16 = 0 < 4.
	npuNode := makeNodeWithNPU("npu-1", 16, nil)

	allPods := []*v1.Pod{makeRunnerPodWithLabels("runner5", "ns1", 4)}
	nodes := []*v1.Node{npuNode}

	fwk, state := setupTestFramework(t, allPods, nodes)
	now := time.Now()
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: map[string]reservation{
			"runner1-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner1", namespace: "ns1"},
			"runner2-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner2", namespace: "ns1"},
			"runner3-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner3", namespace: "ns1"},
			"runner4-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner4", namespace: "ns1"},
		},
	}

	targetPod := makeRunnerPodWithLabels("runner5", "ns1", 4)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Unschedulable {
		t.Errorf("expected Unschedulable (4 reservations × 4 = 16 = full NPU), got %v: %v", status.Code(), status.Message())
	}
}

func TestPreFilterAllowsRunnerWhenReservationsBelowNPU(t *testing.T) {
	// NPU node has 16 NPU. 3 runner pods reserved 4 each on the NPU node
	// (total 12). The 4th runner (needs 4) should pass: per-node free = 16 − 12 = 4.
	npuNode := makeNodeWithNPU("npu-1", 16, nil)

	allPods := []*v1.Pod{makeRunnerPodWithLabels("runner4", "ns1", 4)}
	nodes := []*v1.Node{npuNode}

	fwk, state := setupTestFramework(t, allPods, nodes)
	now := time.Now()
	pl := &ARCSync{
		handle:               fwk,
		inFlightReservations: map[string]reservation{
			"runner1-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner1", namespace: "ns1"},
			"runner2-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner2", namespace: "ns1"},
			"runner3-uid": {nodeName: "npu-1", count: 4, timestamp: now, baseName: "runner3", namespace: "ns1"},
		},
	}

	targetPod := makeRunnerPodWithLabels("runner4", "ns1", 4)
	_, status := pl.PreFilter(context.TODO(), state, targetPod)
	if status.Code() != framework.Success {
		t.Errorf("expected Success (per-node free = 16 − 12 = 4 >= 4), got %v: %v", status.Code(), status.Message())
	}
}
