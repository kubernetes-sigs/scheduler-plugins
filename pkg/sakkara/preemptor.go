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
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/ibm/chic-sched/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Preemptor : Group placement preemptor
type Preemptor struct {
	// priority of job attempting preemption (>0)
	preemptingPriority int

	// list of all nodes in the cluster, as opposed to subset of nodes selected by scheduler for a pod,
	// as preemting groups on unselected nodes may be a good fit for preempting group
	nodes []*v1.Node

	// list of existing job priorities in ascending order (entries < preemptingPriority)
	priorityList []int

	// map of priority -> a map of group infos (by group name)
	priorityGroupMap map[int]map[string]*groupInfo

	// map of node names -> list of (non-group or high priority) pods on node
	podListMap map[string][]*v1.Pod

	// list of victim (to be preempted) groups
	victimGroups []*groupInfo
}

// groupInfo : info about an already placed group
type groupInfo struct {
	name      string
	size      int
	demand    *util.Allocation
	nodeNames []string
	pods      []*v1.Pod
}

// NewPreemptor : create a new preemptor
func NewPreemptor(preemptingPriority int) (*Preemptor, error) {
	if preemptingPriority <= 0 {
		return nil, fmt.Errorf("preempting priority should be larger than zero: %d", preemptingPriority)
	}
	preemptor := &Preemptor{
		preemptingPriority: preemptingPriority,
		nodes:              make([]*v1.Node, 0),
		priorityList:       make([]int, 0),
		priorityGroupMap:   make(map[int]map[string]*groupInfo),
		podListMap:         make(map[string][]*v1.Pod),
	}
	return preemptor, nil
}

// Initialize : initial lists and maps needed to perform preemption
func (preemptor *Preemptor) Initialize() (err error) {
	// get all nodes in the cluster
	topologyData := topologyManager.GetTopologyData()
	if topologyData == nil {
		return fmt.Errorf("failed to get topology data")
	}

	// TODO: exclude filtered out nodes for reasons other than resources fit
	if preemptor.nodes, err = GetAllNodes(); err != nil {
		return fmt.Errorf("failed to get nodes: %s", err.Error())
	}

	// determine pods that are either non-group pods or belong to a group with same or higher priority than that of the preempting group
	for _, node := range preemptor.nodes {
		// get node info
		nodeName := node.GetName()
		nodeInfo, err := handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
		if err != nil {
			return fmt.Errorf("failed to get nodeInfo for node %s: %s", nodeName, err.Error())
		}

		// calculate demand
		for _, pf := range nodeInfo.Pods {
			pod := pf.Pod
			groupName := GetGroupNameFromPod(pod)
			if len(groupName) == 0 {
				klog.V(6).InfoS("Initialize: empty group name, skipping", "pod", pod.GetName())
				preemptor.addToPodsList(nodeName, pod)
				continue
			}
			groupKey := CreateGroupKey(groupName, pod.Namespace)
			groupData := groupManager.GetGroupData(groupKey)
			if groupData == nil {
				klog.V(6).InfoS("Initialize: unknown group name, skipping", "group", groupKey, "pod", pod.GetName())
				preemptor.addToPodsList(nodeName, pod)
				continue
			}

			groupPriority := groupData.Priority
			if groupPriority >= preemptor.preemptingPriority {
				preemptor.addToPodsList(nodeName, pod)
				continue
			}

			if !elementInSlice(preemptor.priorityList, groupPriority) {
				preemptor.priorityList = appendAndSort(preemptor.priorityList, groupPriority)
			}

			groupSize := groupData.Size
			demand, err := GetGroupResourceDemand(pod, topologyData)
			if err != nil {
				return fmt.Errorf("failed to get resource demand for pod %s: %s", pod.GetName(), err.Error())
			}

			groupInfoMap := preemptor.priorityGroupMap[groupPriority]
			if groupInfoMap == nil {
				groupInfoMap = make(map[string]*groupInfo)
				preemptor.priorityGroupMap[groupPriority] = groupInfoMap
			}
			groupKeyStr := groupKey.String()
			gi := groupInfoMap[groupKeyStr]
			if gi == nil {
				groupInfoMap[groupKeyStr] = &groupInfo{
					name:      groupName,
					size:      groupSize,
					demand:    demand,
					nodeNames: []string{nodeName},
					pods:      []*v1.Pod{pod},
				}
			} else {
				gi.nodeNames = append(gi.nodeNames, nodeName)
				gi.pods = append(gi.pods, pod)
			}
		}
	}
	return nil
}

// Preempt : determine victim groups
func (preemptor *Preemptor) Preempt(pod *v1.Pod) (victimGroupNames []string, err error) {
	victimGroupNames = []string{}
	solver, err := NewSolver()
	if err != nil {
		return victimGroupNames, err
	}
	solver.SetPreempting(preemptor.podListMap)
	if err = solver.CreateProblemInstance(pod, preemptor.nodes); err != nil {
		return victimGroupNames, err
	}
	var nodeNames []string
	if nodeNames, err = solver.Solve(); err != nil {
		return victimGroupNames, err
	}
	klog.V(6).InfoS("Preempt: ", "nodeNames", nodeNames)

	// attempt to put back victim groups in order from high to low priorities
	n := len(preemptor.priorityList)
	victimGroups := []*groupInfo{}
	for i := n - 1; i >= 0; i-- {
		priority := preemptor.priorityList[i]
		priorityGroupMap := preemptor.priorityGroupMap[priority]
		if priorityGroupMap == nil {
			continue
		}
		for groupKeyStr, groupInfo := range priorityGroupMap {
			demand := groupInfo.demand
			nodeNames := groupInfo.nodeNames
			if solver.CanPlace(demand, nodeNames) {
				solver.Place(demand, nodeNames)
				klog.V(6).InfoS("Preempt: placed group back", "group", groupKeyStr)
				klog.V(6).InfoS("Preempt: ", "pTree", solver.pTree)
			} else {
				victimGroups = append(victimGroups, groupInfo)
				victimGroupNames = append(victimGroupNames, groupKeyStr)
				klog.V(6).InfoS("Preempt: failed to place group back", "group", groupKeyStr)
			}
		}
	}
	preemptor.victimGroups = victimGroups
	klog.V(6).InfoS("Preempt: ", "victimGroupNames", victimGroupNames)
	return victimGroupNames, nil
}

// DeleteVictimGroups : delete all pods of victim groups
func (preemptor *Preemptor) DeleteVictimGroups() {
	for i := 0; i < len(preemptor.victimGroups); i++ {
		pods := preemptor.victimGroups[i].pods
		for _, pod := range pods {
			klog.V(6).InfoS("Preempt: deleting pod", "pod", pod.GetName())
			handle.ClientSet().CoreV1().Pods(pod.GetNamespace()).Delete(context.Background(),
				pod.GetName(), metav1.DeleteOptions{})
		}
	}
	preemptor.victimGroups = []*groupInfo{}
}

// addToPodsList : add a pod to the list of pods on a given node
func (preemptor *Preemptor) addToPodsList(nodeName string, pod *v1.Pod) {
	if podList, exists := preemptor.podListMap[nodeName]; !exists {
		preemptor.podListMap[nodeName] = []*v1.Pod{pod}
	} else {
		preemptor.podListMap[nodeName] = append(podList, pod)
	}
}

func (preemptor *Preemptor) String() string {
	var b bytes.Buffer
	b.WriteString("Preemptor: \n")
	fmt.Fprintf(&b, "\t preemptingPriority=%d \n", preemptor.preemptingPriority)

	fmt.Fprintf(&b, "\t priorityList: %v \n", preemptor.priorityList)

	b.WriteString("\t priorityGroupMap: \n")
	for priority, groupInfoMap := range preemptor.priorityGroupMap {
		fmt.Fprintf(&b, "\t\t %d: \n", priority)
		for groupKey, groupInfo := range groupInfoMap {
			fmt.Fprintf(&b, "\t\t\t %s: ", groupKey)
			fmt.Fprintf(&b, "name=%s, ", groupInfo.name)
			fmt.Fprintf(&b, "size=%d, ", groupInfo.size)
			fmt.Fprintf(&b, "demand=%v, ", groupInfo.demand)
			fmt.Fprintf(&b, "nodeNames=%v, ", groupInfo.nodeNames)
			b.WriteString("pods= ")
			for _, p := range groupInfo.pods {
				fmt.Fprintf(&b, "%s ", p.GetName())
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func elementInSlice(s []int, e int) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

func appendAndSort(s []int, e int) []int {
	r := append(s, e)
	sort.Ints(r)
	return r
}
