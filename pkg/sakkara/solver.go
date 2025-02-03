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
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ibm/chic-sched/pkg/placement"
	"github.com/ibm/chic-sched/pkg/topology"
	"github.com/ibm/chic-sched/pkg/util"
)

// Solver : Group placement solver
type Solver struct {
	// the topology tree
	pTree *topology.PTree
	// the placement group
	pg *placement.PGroup

	// mode of solver
	isPreempting bool
	// in preempting mode, map from node names to high (>= preempting) priority pods on nodes
	podListMap map[string][]*v1.Pod
}

// NewSolver : create a new solver
func NewSolver() (*Solver, error) {
	solver := &Solver{
		pTree:        nil,
		pg:           nil,
		isPreempting: false,
	}
	return solver, nil
}

// CreateProblemInstance : create an instance of a group placement problem given a set of nodes
func (solver *Solver) CreateProblemInstance(pod *v1.Pod, nodes []*v1.Node) error {
	// create topology tree for given nodes
	pTree, err := topologyManager.GetTopologyForNodes(nodes, solver.CreatePodListMapFor(nodes))
	if err != nil {
		return err
	}
	solver.pTree = pTree

	klog.V(6).InfoS("CreateProblemInstance: ", "pTree", pTree)

	pg := groupManager.CreatePlacementGroup(pod)
	if pg == nil {
		if pg, err = CreatePlacementGroupFromLabels(pod); err != nil {
			return err
		}
	}
	solver.pg = pg

	klog.V(6).InfoS("CreateProblemInstance: ", "pg", pg)
	return nil
}

// Solve : solve the instance of a group placement problem
func (solver *Solver) Solve() ([]string, error) {
	placer := placement.NewPlacer(solver.pTree)
	_, err := placer.PlaceGroup(solver.pg)
	if err != nil {
		return nil, err
	}
	if !solver.pg.IsFullyPlaced() {
		return nil, fmt.Errorf("group %s partially placed", solver.pg.GetID())
	}
	solver.pg.ClaimAll(solver.pTree)

	constraintIDs := solver.pg.GetLevelConstraintIDs()
	klog.V(5).InfoS("Solve: topology group placement problem", "pTree", solver.pTree, "pg", solver.pg,
		"lcs", constraintIDs)
	for j := range constraintIDs {
		klog.V(5).InfoS("Solve: topology group constraints", "levelConstraint", solver.pg.GetLevelConstraint(j))
	}

	les := solver.pg.GetLEGroup().GetLEs()
	numLes := len(les)
	nodeNames := make([]string, numLes)
	for i, le := range les {
		nodeNames[i] = le.GetHost().GetID()
	}
	return nodeNames, nil
}

// CanPlace : check if a demand fits on all of a set of nodes
func (solver *Solver) CanPlace(demand *util.Allocation, nodeNames []string) bool {
	placed := make([]bool, len(nodeNames))
	pTree := solver.pTree
	if pTree == nil {
		return false
	}

	pes := pTree.GetPEs()
	defer func() {
		for i, p := range placed {
			if p {
				pe := pes[nodeNames[i]]
				pe.GetAllocated().Subtract(demand)
			}
		}
	}()

	for i, name := range nodeNames {
		pe := pes[name]
		if pe == nil {
			return false
		}
		if !demand.Fit(pe.GetAllocated(), pe.GetCapacity()) {
			return false
		}
		pe.AddAllocated(demand)
		placed[i] = true
	}
	return true
}

// Place : place demand on all of a set of nodes
func (solver *Solver) Place(demand *util.Allocation, nodeNames []string) {
	pTree := solver.pTree
	pes := pTree.GetPEs()
	for _, name := range nodeNames {
		pe := pes[name]
		if pe != nil {
			pe.AddAllocated(demand)
		}
	}
	pTree.PercolateResources()
}

// SetPreempting : set preempting mode
func (solver *Solver) SetPreempting(podListMap map[string][]*v1.Pod) {
	solver.isPreempting = true
	solver.podListMap = podListMap
}

// CreatePodListMapFor : create a map from node names to pods for a subset of nodes.
// If in preempt mode, only high priority or non-group pods are included,
// otherwise, all pods are included.
func (solver *Solver) CreatePodListMapFor(nodes []*v1.Node) (podListMap map[string][]*v1.Pod) {
	podListMap = make(map[string][]*v1.Pod)
	if solver.isPreempting {
		for _, node := range nodes {
			nodeName := node.GetName()
			if podList, exists := solver.podListMap[nodeName]; exists {
				podListMap[nodeName] = podList
			}
		}
	} else {
		for _, node := range nodes {
			nodeName := node.GetName()
			nodeInfo, err := handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
			if err != nil {
				klog.V(6).InfoS("GetPodListMap: failed to get nodeInfo, skipping", "nodeName", nodeName)
				continue
			}
			podInfosOnNode := nodeInfo.Pods
			podList := make([]*v1.Pod, len(podInfosOnNode))
			for i, pf := range podInfosOnNode {
				podList[i] = pf.Pod
			}
			podListMap[nodeName] = podList
		}
	}
	return podListMap
}
