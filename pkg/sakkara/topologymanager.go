/*
Copyright 2023 The Kubernetes Authors.

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

package sakkara

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"

	"github.com/ibm/chic-sched/pkg/system"
	"github.com/ibm/chic-sched/pkg/topology"
	"github.com/ibm/chic-sched/pkg/util"
)

// TopologyManager : Manager of topology tree
type TopologyManager struct {
	data *TopologyData

	// to serialize changes to topology data
	sync.RWMutex
}

// TopologyData : data related to the topology tree (static specification data)
type TopologyData struct {
	// unique name of tree
	TreeName string
	// names of resources, order of allocation vector in PNodes
	ResourceNames []string
	// names of levels, ordered bottom up, level 0 are leaf nodes
	LevelNames []string
	// the physical tree
	Tree *topology.PTree
}

// NewTopologyManager : create new topology manager
func NewTopologyManager() *TopologyManager {
	return &TopologyManager{
		data: nil,
	}
}

// GetPTree : get the stored PTree instance
func (tm *TopologyManager) GetPTree() *topology.PTree {
	tm.RLock()
	defer tm.RUnlock()

	if data := tm.data; data != nil {
		return data.Tree
	}
	return nil
}

// SetPTree : set the PTree instance
func (tm *TopologyManager) SetPTree(pTree *topology.PTree) {
	tm.Lock()
	defer tm.Unlock()

	if tm.data == nil {
		tm.data = &TopologyData{
			TreeName:      DefaultTreeName,
			ResourceNames: DefaultResourceNames,
			LevelNames:    DefaultLevelNames,
			Tree:          nil,
		}
	}
	tm.data.Tree = pTree
}

// GetTopologyData : get the topology data
func (tm *TopologyManager) GetTopologyData() *TopologyData {
	tm.RLock()
	defer tm.RUnlock()

	return tm.data
}

// SetTopologyData : set the topology data
func (tm *TopologyManager) SetTopologyData(topologyData *TopologyData) {
	tm.Lock()
	defer tm.Unlock()

	tm.data = topologyData
}

// CreateTopology : create the tree topology
func (tm *TopologyManager) CreateTopology() (topologyData *TopologyData, err error) {
	// create topology tree from specs
	if topologyData, err = CreateTreeFromSpec(); err != nil {

		klog.V(6).InfoS("CreateTopology: failed to find topology specs, checking node labels ...")

		// attempt to create using node labels
		var nodes []*v1.Node
		if nodes, err = GetAllNodes(); err != nil {
			return nil, err
		}
		if topologyData, err = CreateTreeFromLabels(nodes); err != nil {

			klog.V(6).InfoS("CreateTopology: failed to find topology node labels, assuming a flat tree ...")

			// if all fails, create flat topology
			nodeNames := make([]string, len(nodes))
			for i, n := range nodes {
				nodeNames[i] = n.GetName()
			}
			topologyData = CreateFlatTree(nodeNames)
		}
	}

	klog.V(6).InfoS("CreateTopology: ", "pTree", topologyData.Tree)

	tm.SetTopologyData(topologyData)
	return topologyData, nil
}

// CreateTreeFromSpec : create the tree topology data by getting the config map
func CreateTreeFromSpec() (*TopologyData, error) {

	klog.V(6).InfoS("CreateTreeFromSpec: getting the topology tree from config map")

	// get the config map
	client := handle.ClientSet().CoreV1().ConfigMaps(TopologyConfigMapNameSpace)
	cmList, err := client.List(context.Background(), metav1.ListOptions{LabelSelector: TopologyConfigMapLabel})
	if err != nil || cmList == nil || len(cmList.Items) == 0 {
		return nil, fmt.Errorf("missing topology config map")
	}
	cm := &(cmList.Items[0])
	return CreateTreeFromConfigMap(cm)
}

// CreateTreeFromConfigMap : create the tree topology data from a config map
func CreateTreeFromConfigMap(cm *v1.ConfigMap) (*TopologyData, error) {

	klog.V(6).InfoS("CreateTreeFromConfigMap: creating topology data", "cm", cm.GetName())

	topologyData := &TopologyData{
		TreeName:      DefaultTreeName,
		ResourceNames: DefaultResourceNames,
		LevelNames:    DefaultLevelNames,
		Tree:          nil,
	}

	// get tree name
	if tn, exists := cm.Data[TopologyConfigMapTreeNameKey]; exists {
		topologyData.TreeName = tn
	}

	// get resource names
	if rnStr, exists := cm.Data[TopologyConfigMapResourceNamesKey]; exists {
		rn := make([]string, 0)
		if err := json.Unmarshal([]byte(rnStr), &rn); err != nil {
			return nil, fmt.Errorf("error parsing resource names: %s", err.Error())
		}

		klog.V(6).InfoS("CreateTreeFromConfigMap: ", "resourceNames", rn)

		topologyData.ResourceNames = rn
	}
	numResources := len(topologyData.ResourceNames)

	// get level names
	if lnStr, exists := cm.Data[TopologyConfigMapLevelNamesKey]; exists {
		ln := make([]string, 0)
		if err := json.Unmarshal([]byte(lnStr), &ln); err != nil {
			return nil, fmt.Errorf("error parsing level names: %s", err.Error())
		}

		klog.V(6).InfoS("CreateTreeFromConfigMap: ", "levelNames", ln)

		// reverse levels
		numLevels := len(ln)
		topologyData.LevelNames = make([]string, numLevels)
		for i := range ln {
			topologyData.LevelNames[i] = ln[numLevels-1-i]
		}
	}

	// get tree topology
	treeString := cm.Data[TopologyConfigMapTreeKey]
	if len(treeString) == 0 {
		return nil, fmt.Errorf("missing topology tree spec")
	}

	klog.V(6).InfoS("CreateTreeFromConfigMap: ", "treeString", treeString)

	treeMap := TreeSpec{}
	err := json.Unmarshal([]byte(treeString), &treeMap)
	if err != nil {
		return nil, fmt.Errorf("error parsing tree topology: %s", err.Error())
	}

	// make PTree
	root := topology.NewPNode(topology.NewNode(&system.Entity{ID: "root"}), 0, numResources)
	MakeSubtreeFromSpec(root, treeMap)
	pTree := topology.NewPTree(topology.NewTree((*topology.Node)(unsafe.Pointer(root))))
	pTree.SetNodeLevels()
	topologyData.Tree = pTree

	klog.V(6).InfoS("CreateTreeFromConfigMap: ", "pTree", pTree)

	return topologyData, nil
}

// MakeSubtreeFromSpec : make a substree rooted at a node given tree spec level hierarchy,
// without setting level values
func MakeSubtreeFromSpec(pNode *topology.PNode, spec TreeSpec) {
	numResources := pNode.GetNumResources()
	for childName, childSpec := range spec {
		child := topology.NewPNode(topology.NewNode(&system.Entity{ID: childName}), 0, numResources)
		pNode.AddChild((*topology.Node)(unsafe.Pointer(child)))
		MakeSubtreeFromSpec(child, childSpec)
	}
}

// CreateTreeFromLabels : Assumes two level tree (racks, servers)
func CreateTreeFromLabels(nodes []*v1.Node) (*TopologyData, error) {

	klog.V(6).InfoS("CreateTreeFromLabels: attempting to create topology tree from node labels", "numNodes", len(nodes))

	topologyData := &TopologyData{}
	topologyData.TreeName = DefaultTreeName
	topologyData.ResourceNames = DefaultResourceNames
	topologyData.LevelNames = DefaultLevelNames
	numResources := len(DefaultResourceNames)
	numLevels := len(DefaultLevelNames)

	root := topology.NewPNode(topology.NewNode(&system.Entity{ID: "root"}), numLevels, numResources)
	rackMap := make(map[string]*topology.PNode)

	// visit nodes and build tree based on node labels
	for _, node := range nodes {
		// get rack ID
		rackID := node.GetLabels()[TopologyRackKey]
		if len(rackID) == 0 {
			//rackID = "rack0"

			klog.V(6).InfoS("CreateTreeFromLabels: missing rack ID", "node", node.GetName())
			return nil, fmt.Errorf("missing topology node labels")

		}
		// create rack node if does not exist
		rack := rackMap[rackID]
		if rack == nil {
			rack = topology.NewPNode(topology.NewNode(&system.Entity{ID: rackID}), 1, numResources)
			rackMap[rackID] = rack
			root.AddChild(&rack.Node)
		}
		server := topology.NewPNode(topology.NewNode(&system.Entity{ID: node.GetName()}), 0, numResources)
		rack.AddChild(&server.Node)

		klog.V(6).InfoS("CreateTreeFromLabels: ", "server", server)
	}
	// create PTree
	topologyData.Tree = topology.NewPTree(topology.NewTree(&root.Node))
	return topologyData, nil
}

// CreateFlatTree : create a flat PTree
func CreateFlatTree(leafIDs []string) (topologyData *TopologyData) {

	klog.V(6).InfoS("CreateFlatTree: creating a flat topology tree", "numNodes", len(leafIDs))

	topologyData = &TopologyData{}
	topologyData.TreeName = DefaultTreeName
	topologyData.ResourceNames = DefaultResourceNames
	numResources := len(DefaultResourceNames)
	topologyData.LevelNames = []string{DefaultLevelNames[0]}

	root := topology.NewPNode(topology.NewNode(&system.Entity{ID: "root"}), 0, numResources)
	root.SetLevel(1)
	for _, id := range leafIDs {
		leaf := topology.NewPNode(topology.NewNode(&system.Entity{ID: id}), 0, numResources)
		leaf.SetLevel(0)
		root.AddChild((*topology.Node)(unsafe.Pointer(leaf)))
	}
	topologyData.Tree = topology.NewPTree(topology.NewTree((*topology.Node)(unsafe.Pointer(root))))
	return topologyData
}

// GetTopologyForNodes : Subset of pTree containing only a set of leaf nodes, and pods to include in topology allocation
func (tm *TopologyManager) GetTopologyForNodes(nodes []*v1.Node, podListMap map[string][]*v1.Pod) (pTree *topology.PTree, err error) {
	topologyData := tm.GetTopologyData()
	// create topology if none stored
	if topologyData == nil {
		if topologyData, err = tm.CreateTopology(); err != nil {
			return nil, err
		}
		tm.SetTopologyData(topologyData)
	}
	// create leaf nodes (PEs)
	resourceNames := topologyData.ResourceNames
	peMap, peList, err := CreatePEsForNodes(nodes, resourceNames, podListMap)
	if err != nil {
		return nil, fmt.Errorf("failed to create PEs")
	}
	// connect and prime pTree
	tree := topologyData.Tree
	pTree = tree.CopyByLeafIDs(peList)
	pTree.SetPEs(peMap)
	pTree.PercolateResources()
	return pTree, nil
}

// CreatePEsForNodes : create PE objects corresponding to a set of nodes, and pods to include for allocation
func CreatePEsForNodes(nodes []*v1.Node, resourceNames []string,
	podListMap map[string][]*v1.Pod) (peMap map[string]*system.PE, peList []string, err error) {

	numServers := len(nodes)
	peMap = make(map[string]*system.PE)
	peList = make([]string, numServers)
	for i, node := range nodes {
		// create PE node
		nodeName := node.GetName()
		pe, err := CreatePE(node, resourceNames, podListMap[nodeName])
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create PE for node %s", nodeName)
		}
		peMap[nodeName] = pe
		peList[i] = nodeName

		klog.V(6).InfoS("CreatePEsForNodes: ", "pe", pe)
	}
	return peMap, peList, nil
}

// CreatePE : create a PE object corresponding to a node, and pods to include for allocation
func CreatePE(node *v1.Node, resourceNames []string, podList []*v1.Pod) (*system.PE, error) {
	nodeName := node.GetName()

	// get node capacity
	capacity, err := util.NewAllocationCopy(getNodeCapacity(node, resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create capacity allocation for node %s", nodeName)
	}

	// calculate node allocation
	allocated, err := util.NewAllocationCopy(getNodeAllocation(resourceNames, podList))
	if err != nil {
		return nil, fmt.Errorf("failed to create requests allocation for node %s", nodeName)
	}

	pe := system.NewPE(nodeName, capacity)
	pe.SetAllocated(allocated)
	return pe, nil
}

// getNodeCapacity : get node capacity
func getNodeCapacity(node *v1.Node, resourceNames []string) []int {
	allocatableResources := node.Status.Allocatable
	cap := make([]int, len(resourceNames))
	for k, resourceName := range resourceNames {
		switch resourceName {
		case v1.ResourceCPU.String():
			{
				amCpu := allocatableResources[v1.ResourceCPU]
				cap[k] = int(amCpu.MilliValue())
			}
		default:
			{
				am := allocatableResources[v1.ResourceName(resourceName)]
				cap[k] = int(am.Value())
			}
		}
	}
	return cap
}

// getNodeAllocation : get allocation of pods to include for allocation on a node
func getNodeAllocation(resourceNames []string, podList []*v1.Pod) []int {
	alloc := make([]int, len(resourceNames))
	for _, pod := range podList {
		requests := trimaran.GetResourceRequested(pod)
		for k, resourceName := range resourceNames {
			switch resourceName {
			case v1.ResourceCPU.String():
				alloc[k] += int(requests.MilliCPU)
			case v1.ResourceMemory.String():
				alloc[k] += int(requests.Memory)
			case v1.ResourceEphemeralStorage.String():
				alloc[k] += int(requests.EphemeralStorage)
			case v1.ResourcePods.String():
				alloc[k] += 1
			default:
				alloc[k] += int(requests.ScalarResources[v1.ResourceName(resourceName)])
			}
		}
	}
	return alloc
}

// GetAllNodes : get all nodes in the cluster through the shared lister
func GetAllNodes() ([]*v1.Node, error) {
	nodeInfos, err := handle.SnapshotSharedLister().NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("failed to get nodeInfos")
	}
	n := len(nodeInfos)
	if n == 0 {
		return nil, fmt.Errorf("zero nodes available")
	}
	nodes := make([]*v1.Node, n)
	for i := range nodeInfos {
		nodes[i] = nodeInfos[i].Node()
	}
	return nodes, nil
}

// GetBoundTree : create placement tree from node names to pods map
func (tm *TopologyManager) GetBoundTree(nodeNamesToPodNames map[string][]string) *topology.Tree {
	numLeaves := len(nodeNamesToPodNames)
	leafIDs := make([]string, numLeaves)
	i := 0
	for k := range nodeNamesToPodNames {
		leafIDs[i] = k
		i++
	}

	tm.RLock()
	defer tm.RUnlock()

	if tm.data == nil || tm.data.Tree == nil {
		return nil
	}

	pTree := tm.data.Tree
	tree := (*topology.Tree)(unsafe.Pointer(pTree))
	bTree := tree.CopyByLeafIDs(leafIDs)
	if bTree == nil {
		return nil
	}

	for nodeName, node := range bTree.GetLeavesMap() {
		for _, podName := range nodeNamesToPodNames[nodeName] {
			podChild := topology.NewNode(&system.Entity{ID: podName})
			node.AddChild(podChild)
		}
	}
	return bTree
}
