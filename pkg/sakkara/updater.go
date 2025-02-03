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
	"strconv"
	"strings"
	"time"

	"github.com/ibm/chic-sched/pkg/topology"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

type patchStringValue struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// TODO: consolidate updates to group configmap
// TODO: check if we need to read lock the group manager while updating

// UpdateGroupConfigMap : update the group configmap with the group logical placement tree and rank values
func UpdateGroupConfigMap(groupKey *GroupKey, group *Group, tree *topology.Tree, podsMap map[string]*PodData) error {

	groupData := groupManager.GetGroupData(groupKey)
	if groupData == nil {
		return fmt.Errorf("UpdateGroupConfigMap: failed to get group data for group %s", groupKey)
	}

	mapTree := tree.ToMap()
	klog.V(6).InfoS("UpdateGroupConfigMap: bound tree", "mapTree =", mapTree)
	treeString, err := json.Marshal(mapTree)
	if err != nil {
		klog.V(6).InfoS("UpdateGroupConfigMap: bound tree", "error", err.Error())
		return err
	}

	leafIDs := tree.GetLeafIDs()
	masterPodName := leafIDs[0]
	for _, id := range leafIDs {
		if strings.Contains(id, GroupConfigMasterSubstring) {
			masterPodName = id
			break
		}
	}
	nodeValues := tree.GetLeavesDistanceFrom(masterPodName)
	for rank, nv := range nodeValues {
		nv.Value = rank
	}
	rankString := fmt.Sprintf("%v", nodeValues)

	// update pods, set rank as label
	for _, nv := range nodeValues {
		if podData, exists := podsMap[nv.Name]; exists {
			patch := []byte("{\"metadata\":{\"labels\":{" +
				"\"" + MemberRankKey + "\":\"" + strconv.Itoa(nv.Value) + "\"" +
				"}}}")

			if _, err := handle.ClientSet().CoreV1().Pods(podData.NameSpace).Patch(context.Background(),
				podData.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
				return err
			}
		}
	}

	// update configmap
	cmData := groupData.CMData
	if cmData == nil {
		return fmt.Errorf("UpdateGroupConfigMap: failed to get configmap data for group %s", groupKey)
	}

	// timestamps
	var beginTimeString, endTimeString, schedTimeString, solverTimeString string
	var beginTime, endTime *time.Time
	if beginTime = group.GetBeginTime(); beginTime != nil {
		beginTimeString = beginTime.Format(time.RFC3339Nano)
	}
	if endTime = group.GetEndTime(); endTime != nil {
		endTimeString = endTime.Format(time.RFC3339Nano)
	}
	if beginTime != nil && endTime != nil {
		schedTimeString = strconv.FormatInt(endTime.Sub(*beginTime).Milliseconds(), 10)
	}
	solverTimeString = strconv.FormatInt(group.GetSolverTime(), 10)

	payload := []patchStringValue{{
		Op:    "replace",
		Path:  "/data/" + GroupConfigMapPlacementKey,
		Value: string(treeString),
	},
		{
			Op:    "replace",
			Path:  "/data/" + GroupConfigMapRankKey,
			Value: rankString,
		},
		{
			Op:    "replace",
			Path:  "/data/" + GroupConfigMapSchedBeginTimeKey,
			Value: beginTimeString,
		},
		{
			Op:    "replace",
			Path:  "/data/" + GroupConfigMapSchedEndTimeKey,
			Value: endTimeString,
		},
		{
			Op:    "replace",
			Path:  "/data/" + GroupConfigMapSchedSolverTimeMilliKey,
			Value: solverTimeString,
		},
		{
			Op:    "replace",
			Path:  "/data/" + GroupConfigMapSchedTotalTimeMilliKey,
			Value: schedTimeString,
		}}
	payloadBytes, _ := json.Marshal(payload)

	_, err = handle.ClientSet().CoreV1().ConfigMaps(groupData.NameSpace).Patch(context.Background(), cmData.Name,
		types.JSONPatchType, payloadBytes, metav1.PatchOptions{})
	if err != nil {
		klog.V(6).InfoS("UpdateGroupConfigMap:", "error", err.Error())
		return err
	}
	return nil
}

// UpdateGroupStatus : update the group status in the group configmap
func UpdateGroupStatus(groupKey *GroupKey, status Status) error {
	groupData := groupManager.GetGroupData(groupKey)
	if groupData == nil {
		return fmt.Errorf("UpdateGroupStatus: failed to get group data for group %s", groupKey)
	}
	cmData := groupData.CMData
	if cmData == nil {
		return fmt.Errorf("UpdateGroupStatus: failed to get configmap data for group %s", groupKey)
	}

	payload := []patchStringValue{{
		Op:    "replace",
		Path:  "/data/" + GroupConfigMapStatusKey,
		Value: StatusName[status],
	}}
	payloadBytes, _ := json.Marshal(payload)

	if _, err := handle.ClientSet().CoreV1().ConfigMaps(groupData.NameSpace).Patch(context.Background(), cmData.Name,
		types.JSONPatchType, payloadBytes, metav1.PatchOptions{}); err != nil {
		klog.V(6).InfoS("UpdateGroupStatus:", "error", err.Error())
		return err
	}
	return nil
}

// UpdateGroupDelayedDeletion : update the group delayed deletion time in the group configmap
func UpdateGroupDelayedDeletion(groupKey *GroupKey, delayedDeletionTime *time.Time) error {
	groupData := groupManager.GetGroupData(groupKey)
	if groupData == nil {
		return fmt.Errorf("UpdateGroupDelayedDeletion: failed to get group data for group %s", groupKey)
	}
	cmData := groupData.CMData
	if cmData == nil {
		return fmt.Errorf("UpdateGroupDelayedDeletion: failed to get configmap data for group %s", groupKey)
	}

	op := "remove"
	value := ""
	if delayedDeletionTime != nil {
		op = "replace"
		value = delayedDeletionTime.Format(time.RFC3339)
	}

	payload := []patchStringValue{{
		Op:    op,
		Path:  "/data/" + GroupConfigMapDelayedDeletionKey,
		Value: value,
	}}
	payloadBytes, _ := json.Marshal(payload)

	if _, err := handle.ClientSet().CoreV1().ConfigMaps(groupData.NameSpace).Patch(context.Background(), cmData.Name,
		types.JSONPatchType, payloadBytes, metav1.PatchOptions{}); err != nil {
		klog.V(6).InfoS("UpdateGroupDelayedDeletion:", "error", err.Error())
		return err
	}
	return nil
}

// UpdateGroupPreemptor : update the group preemptor in the group configmap
func UpdateGroupPreemptor(groupKey *GroupKey, preemptor string) error {
	groupData := groupManager.GetGroupData(groupKey)
	if groupData == nil {
		return fmt.Errorf("UpdateGroupPreemptor: failed to get group data for group %s", groupKey)
	}
	cmData := groupData.CMData
	if cmData == nil {
		return fmt.Errorf("UpdateGroupPreemptor: failed to get configmap data for group %s", groupKey)
	}

	klog.V(6).InfoS("UpdateGroupPreemptor: ", "group", groupKey, "preemptor", preemptor)

	op := "remove"
	value := ""
	if len(preemptor) > 0 {
		op = "replace"
		value = preemptor
	}

	payload := []patchStringValue{{
		Op:    op,
		Path:  "/data/" + GroupConfigMapPreemptorKey,
		Value: value,
	}}
	payloadBytes, _ := json.Marshal(payload)

	if _, err := handle.ClientSet().CoreV1().ConfigMaps(groupData.NameSpace).Patch(context.Background(), cmData.Name,
		types.JSONPatchType, payloadBytes, metav1.PatchOptions{}); err != nil {
		klog.V(6).InfoS("UpdateGroupPreemptor:", "error", err.Error())
		return err
	}
	return nil
}
