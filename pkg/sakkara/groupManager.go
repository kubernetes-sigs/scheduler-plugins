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
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/ibm/chic-sched/pkg/placement"
	"github.com/ibm/chic-sched/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

// GroupManager : Manager of placement groups
type GroupManager struct {
	// map of all groups from configmaps: groupKeyStr -> groupData
	groupDataMap map[string]*GroupData

	// to serialize operations on the groupDataMap
	sync.RWMutex
}

// GroupData : placement data about a group (static specification data)
type GroupData struct {
	Name        string
	NameSpace   string
	Size        int
	Priority    int
	Constraints map[string]*ConstraintData
	CMData      *ConfigMapData
}

// ConfigMapData : data relevant to group config map
type ConfigMapData struct {
	UID  types.UID
	Name string
}

// ConstraintData : data about a level constraint
type ConstraintData struct {
	Type       string `json:"type"`
	Hard       bool   `json:"hard"`
	Partitions int    `json:"partitions"`
	Min        int    `json:"min"`
	Max        int    `json:"max"`
	Factor     int    `json:"factor"`
}

// NewGroupManager : create new group manager
func NewGroupManager() *GroupManager {
	return &GroupManager{
		groupDataMap: make(map[string]*GroupData),
	}
}

// GetGroupData : get the group data for a group
func (gm *GroupManager) GetGroupData(groupKey *GroupKey) *GroupData {
	gm.RLock()
	defer gm.RUnlock()

	return gm.groupDataMap[groupKey.String()]
}

// SetGroupData : set the group data for a group
func (gm *GroupManager) SetGroupData(groupData *GroupData) {
	gm.Lock()
	defer gm.Unlock()

	gm.groupDataMap[groupData.GroupKey().String()] = groupData
}

// DeleteGroupData : delete the group data for a group
func (gm *GroupManager) DeleteGroupData(groupKey *GroupKey) {
	gm.Lock()
	defer gm.Unlock()

	delete(gm.groupDataMap, groupKey.String())
}

// GroupExists : check if group exists
func (gm *GroupManager) GroupExists(groupKey *GroupKey) bool {
	gm.RLock()
	defer gm.RUnlock()

	return gm.groupDataMap[groupKey.String()] != nil
}

// GetGroupSize : get the size of a group (zero if does not exist)
func (gm *GroupManager) GetGroupSize(groupKey *GroupKey) int {
	gm.RLock()
	defer gm.RUnlock()

	if gd := gm.groupDataMap[groupKey.String()]; gd != nil {
		return gd.Size
	}
	return 0
}

// GetGroupPriority : get the priority of a group (zero if does not exist)
func (gm *GroupManager) GetGroupPriority(groupKey *GroupKey) int {
	gm.RLock()
	defer gm.RUnlock()

	if gd := gm.groupDataMap[groupKey.String()]; gd != nil {
		return gd.Priority
	}
	return 0
}

// CreateGroupFromConfigMap : create the group data from a config map
func CreateGroupFromConfigMap(cm *v1.ConfigMap) (*GroupData, error) {

	klog.V(6).InfoS("CreateGroupFromConfigMap: ", "cm", cm.GetName())

	groupData := &GroupData{
		Name:        DefaultGroupName,
		NameSpace:   cm.GetNamespace(),
		Size:        DefaultGroupSize,
		Priority:    DefaultGroupPriority,
		Constraints: make(map[string]*ConstraintData),
		CMData: &ConfigMapData{
			UID:  cm.GetUID(),
			Name: cm.GetName(),
		},
	}

	// get group name
	if gn, exists := cm.Data[GroupConfigMapGroupNameKey]; exists {
		groupData.Name = gn
	}

	// get group size
	if gs, exists := cm.Data[GroupConfigMapGroupSizeKey]; exists {
		if size, err := strconv.Atoi(gs); err == nil {
			groupData.Size = size
		}
	}

	// get group priority
	if gp, exists := cm.Data[GroupConfigMapGroupPriorityKey]; exists {
		if priority, err := strconv.Atoi(gp); err == nil {
			groupData.Priority = priority
		}
	}

	// get group constraints
	groupConstraintsString := cm.Data[GroupConfigMapGroupConstraintsKey]
	if err := json.Unmarshal([]byte(groupConstraintsString), &groupData.Constraints); err != nil {
		klog.V(6).InfoS("CreateGroupFromConfigMap: ", "err", err)
		return nil, fmt.Errorf("error parsing placement group constraints: %s", err.Error())
	}

	klog.V(6).InfoS("CreateGroupFromConfigMap: ", "groupData", groupData)

	return groupData, nil
}

// GetGroupNameFromConfigMap : get group name if exists, or default name
func GetGroupNameFromConfigMap(cm *v1.ConfigMap) string {
	if name, exists := cm.Data[GroupConfigMapGroupNameKey]; exists {
		return name
	}
	return DefaultGroupName
}

// CreatePlacementGroup : create a placement group
func (gm *GroupManager) CreatePlacementGroup(pod *v1.Pod) *placement.PGroup {
	groupName := GetGroupNameFromPod(pod)
	groupKey := CreateGroupKey(groupName, pod.GetNamespace())
	groupData := gm.GetGroupData(groupKey)
	if groupData == nil {
		klog.V(6).InfoS("CreatePlacementGroup: missing groupData", "group", groupKey)
		return nil
	}

	topologyData := topologyManager.GetTopologyData()
	if topologyData == nil {
		klog.V(6).InfoS("CreatePlacementGroup: failed to get topology data")
		return nil
	}

	// calculate resource demands
	groupDemand, err := GetGroupResourceDemand(pod, topologyData)
	if err != nil {
		klog.V(6).InfoS("CreatePlacementGroup: failed to create requests allocation",
			"group", groupKey, "podName", pod.GetName())
		return nil
	}

	pg := placement.NewPGroup(groupKey.String(), groupData.Size, groupDemand)

	// create level constraints
	for l, levelName := range topologyData.LevelNames {
		constraintData := groupData.Constraints[levelName]

		affinity := util.Pack
		if constraintData != nil && constraintData.Type == "spread" {
			affinity = util.Spread
		}

		isHard := false
		if constraintData != nil && constraintData.Hard {
			isHard = true
		}

		lc := placement.NewLevelConstraint(levelName, l, util.Affinity(affinity), isHard)
		pg.AddLevelConstraint(lc)

		if constraintData == nil {
			continue
		}

		if constraintData.Partitions > 0 {
			lc.SetNumPartitions(constraintData.Partitions)
		}
		if constraintData.Min > 0 && constraintData.Max > 0 && constraintData.Min <= constraintData.Max {
			lc.SetRange(constraintData.Min, constraintData.Max)
		}
		if constraintData.Factor > 1 {
			lc.SetFactor(constraintData.Factor)
		}
	}
	return pg
}

// CreatePlacementGroupFromLabels : create a placement group using labels in pod
func CreatePlacementGroupFromLabels(pod *v1.Pod) (*placement.PGroup, error) {
	topologyData := topologyManager.GetTopologyData()
	if topologyData == nil {
		return nil, fmt.Errorf("failed to get topology data")
	}

	// calculate resource demands
	groupDemand, err := GetGroupResourceDemand(pod, topologyData)
	if err != nil {
		return nil, fmt.Errorf("failed to create requests allocation for pod %s", pod.GetName())
	}

	// create placement group
	groupKey := CreateGroupKey(GetGroupNameFromPod(pod), pod.GetNamespace())
	pg := placement.NewPGroup(groupKey.String(), GetGroupSizeFromPod(pod), groupDemand)

	// create level constraints
	isHard := false
	levelNames := topologyData.LevelNames
	for l, levelName := range levelNames {
		dotLevel := "." + levelName
		affinity := util.Pack
		if aff := pod.Labels[GroupConstraintTypeKey+dotLevel]; len(aff) > 0 {
			switch aff {
			case "pack":
				affinity = util.Pack
			case "spread":
				affinity = util.Spread
			}
		}
		numPartitions, _ := strconv.Atoi(pod.Labels[GroupConstraintPartitionsKey+dotLevel])
		minRange, _ := strconv.Atoi(pod.Labels[GroupConstraintMinRangeKey+dotLevel])
		maxRange, _ := strconv.Atoi(pod.Labels[GroupConstraintMaxRangeKey+dotLevel])
		factor, _ := strconv.Atoi(pod.Labels[GroupConstraintFactorKey+dotLevel])

		lc := placement.NewLevelConstraint("lc-"+strconv.Itoa(l), l, util.Affinity(affinity), isHard)
		pg.AddLevelConstraint(lc)
		if numPartitions > 0 {
			lc.SetNumPartitions(numPartitions)
		}
		if minRange > 0 && maxRange > 0 && minRange <= maxRange {
			lc.SetRange(minRange, maxRange)
		}
		if factor > 1 {
			lc.SetFactor(factor)
		}
	}
	return pg, nil
}

// GetGroupResourceDemand : calculate resource demands from member pod
func GetGroupResourceDemand(pod *v1.Pod, topologyData *TopologyData) (*util.Allocation, error) {
	resourceNames := topologyData.ResourceNames
	numResources := len(resourceNames)
	demand := trimaran.GetResourceRequested(pod)
	demandValues := make([]int, numResources)
	for k, resourceName := range resourceNames {
		switch resourceName {
		case v1.ResourceCPU.String():
			demandValues[k] = int(demand.MilliCPU)
		case v1.ResourceMemory.String():
			demandValues[k] = int(demand.Memory)
		case v1.ResourceEphemeralStorage.String():
			demandValues[k] = int(demand.EphemeralStorage)
		case v1.ResourcePods.String():
			demandValues[k] = 1
		default:
			demandValues[k] = int(demand.ScalarResources[v1.ResourceName(resourceName)])
		}
	}
	return util.NewAllocationCopy(demandValues)
}

// GroupKey : create group key from group data
func (gd *GroupData) GroupKey() *GroupKey {
	return &GroupKey{
		Name:      gd.Name,
		Namespace: gd.NameSpace,
	}
}

// IsConfigMap : is the source of group data a configmap
func (gd *GroupData) IsConfigMap() bool {
	return gd.CMData != nil
}
