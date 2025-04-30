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
	"strings"
)

const (
	// prefix key label
	KeyPrefix = "sakkara."

	// topology tree configmap constants
	DefaultTreeName        = "default-tree"
	TopologyConfigMapLabel = KeyPrefix + "topology"

	// topology tree keys
	TopologyConfigMapTreeNameKey      = "name"
	TopologyConfigMapResourceNamesKey = "resource-names"
	TopologyConfigMapLevelNamesKey    = "level-names"
	TopologyConfigMapTreeKey          = "tree"
	TopologyRackKey                   = KeyPrefix + "topology.rack"

	// group configmap constants
	DefaultGroupName    = "default-group"
	GroupConfigMapLabel = KeyPrefix + "group"

	// group configmap keys
	GroupConfigMapGroupNameKey            = "name"
	GroupConfigMapGroupSizeKey            = "size"
	GroupConfigMapGroupPriorityKey        = "priority"
	GroupConfigMapGroupConstraintsKey     = "constraints"
	GroupConfigMapPlacementKey            = "placement"
	GroupConfigMapRankKey                 = "rank"
	GroupConfigMapStatusKey               = "status"
	GroupConfigMapDelayedDeletionKey      = "delayed-deletion"
	GroupConfigMapPreemptorKey            = "preemptor"
	GroupConfigMapSchedBeginTimeKey       = "schedBeginTime"
	GroupConfigMapSchedEndTimeKey         = "schedEndTime"
	GroupConfigMapSchedTotalTimeMilliKey  = "schedTotalTimeMilli"
	GroupConfigMapSchedSolverTimeMilliKey = "schedSolverTimeMilli"

	// group label keys
	GroupNameKey     = KeyPrefix + "group.name"
	GroupSizeKey     = KeyPrefix + "group.size"
	GroupPriorityKey = KeyPrefix + "group.priority"
	GroupStatusKey   = KeyPrefix + "group.status"

	// member label keys
	MemberStatusKey  = KeyPrefix + "member.status"
	MemberRetriesKey = KeyPrefix + "member.retries"
	MemberRankKey    = KeyPrefix + "member.rank"

	// group constraint label keys
	GroupConstraintTypeKey       = KeyPrefix + "group.constraint.type"
	GroupConstraintPartitionsKey = KeyPrefix + "group.constraint.partitions"
	GroupConstraintMinRangeKey   = KeyPrefix + "group.constraint.min"
	GroupConstraintMaxRangeKey   = KeyPrefix + "group.constraint.max"
	GroupConstraintFactorKey     = KeyPrefix + "group.constraint.factor"

	// group parameters
	DefaultGroupSize                    = 1
	DefaultGroupPriority                = 0
	DelayedGroupDeletionIntervalSeconds = 20
	DelayedGroupDeletionDurationSeconds = 10
	GroupConfigMasterSubstring          = "master"

	// other parameters
	PodDeletedKeepDurationSeconds = 120
	CleanupIntervalSeconds        = 120
)

var (
	DefaultResourceNames = []string{"cpu", "memory"}
	DefaultLevelNames    = []string{"node", "rack"}
)

// TreeSpec : spec for (sub) tree
type TreeSpec map[string]TreeSpec

// GroupKey : data to uniquely identify a group
type GroupKey struct {
	Name      string
	Namespace string
}

// CreateGroupKey : create a group key object
func CreateGroupKey(name string, namespace string) *GroupKey {
	return &GroupKey{
		Name:      name,
		Namespace: namespace,
	}
}

// make a unique key string by juxtaposing namespace and name, separated by slash
func (gk *GroupKey) String() string {
	return gk.Namespace + "/" + gk.Name
}

// CreateGroupKeyFromString : extract name and namespace from key string to create a GroupKey object
func CreateGroupKeyFromString(keyStr string) (*GroupKey, error) {
	if strs := strings.Split(keyStr, "/"); len(strs) == 2 {
		return CreateGroupKey(strs[1], strs[0]), nil
	}
	return nil, fmt.Errorf("error parsing group key: %s", keyStr)
}
