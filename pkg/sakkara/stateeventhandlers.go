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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/slices"
	v1 "k8s.io/api/core/v1"
	clientcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PodEventHandler : event handler for pod members of groups
type PodEventHandler struct {
	// state is modified based on pod changes
	state *State

	sync.RWMutex
}

// NewPodEventHandler : create a new pod event hadler
func NewPodEventHandler(state *State) *PodEventHandler {
	return &PodEventHandler{
		state: state,
	}
}

// AddToHandle : add event handler to framework handle
func (p *PodEventHandler) AddToHandle(handle framework.Handle) {
	p.Lock()
	defer p.Unlock()

	// filter pods with label having prefix "sakkara."
	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(
		clientcache.FilteringResourceEventHandler{
			// TODO: more efficient way to do the selection
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					{
						for k := range t.GetLabels() {
							if strings.HasPrefix(k, KeyPrefix) {
								return true
							}
						}
					}
				default:
				}
				return false
			},
			Handler: p,
		},
	)
}

func (p *PodEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	if !isInInitialList {
		return
	}
	p.Lock()
	defer p.Unlock()

	pod := obj.(*v1.Pod)
	namespace := pod.GetNamespace()
	klog.V(6).InfoS("PodEventHandler: OnAdd()", "pod", namespace+"/"+pod.GetName())
	groupName := GetGroupNameFromPod(pod)
	groupKey := CreateGroupKey(groupName, namespace)
	if group, groupExists := p.state.GetGroup(groupKey); groupExists && !group.IsMember(pod) {
		// add new pod member to group and update state
		member := NewMemberForPod(pod)
		// update status
		if status, statusExists := pod.GetLabels()[MemberStatusKey]; statusExists {
			if index := slices.Index(StatusName, status); index >= 0 && index < len(StatusName) {
				member.SetStatus(Status(index))
			}
		}
		// update node name
		if nodeName := pod.Spec.NodeName; len(nodeName) > 0 {
			member.NodeName.Bound = nodeName
		}
		// update retries
		if retries, retriesExists := pod.GetLabels()[MemberRetriesKey]; retriesExists {
			if numRetries, err := strconv.Atoi(retries); err == nil {
				member.Retries = numRetries
			}
		}
		// add member to group
		if added := group.AddMember(member); added {
			klog.V(6).InfoS("PodEventHandler: OnAdd() added pod as member in group",
				"podName", pod.GetName(), "group", groupKey, "member", member)
		}
	}
}

func (p *PodEventHandler) OnUpdate(oldObj, newObj interface{}) {

}

func (p *PodEventHandler) OnDelete(obj interface{}) {
	p.Lock()
	defer p.Unlock()

	// remove member corresponding to deleted pod from state
	pod := obj.(*v1.Pod)
	namespace := pod.GetNamespace()
	klog.V(6).InfoS("PodEventHandler: OnDelete()", "pod", namespace+"/"+pod.GetName())
	now := time.Now()
	p.state.AddDeletedPod(pod, &now)
	if groupName := GetGroupNameFromPod(pod); len(groupName) > 0 {
		groupKey := CreateGroupKey(groupName, namespace)
		if group, exists := p.state.GetGroup(groupKey); exists {
			if member := group.GetMember(pod); member != nil {
				group.RemoveMember(member)
				if group.GetNumMembers() == 0 {
					groupStatus := group.GetStatus()
					klog.V(6).InfoS("PodEventHandler: OnDelete()", "group", groupKey, "status", groupStatus)
					if groupStatus == Preempted {
						klog.V(6).InfoS("PodEventHandler: Delaying deletion", "group", groupKey)
						group.DelayDeletion()
					} else {
						klog.V(6).InfoS("PodEventHandler: Deleting", "group", groupKey)
						p.state.RemoveGroup(groupKey)

						// cleanup group from group manager if pod group (we're not using an informer on podgroups)
						if groupData := groupManager.GetGroupData(groupKey); groupData != nil && !groupData.IsConfigMap() {
							groupManager.DeleteGroupData(groupKey)
						}
					}
				}
			}
			group.RemoveDelayedPod(string(pod.GetUID()))
		}
	}
}

// ConfigMapEventHandler : event handler for topology and group config maps
type ConfigMapEventHandler struct {
	// state is modified for new groups
	state *State

	sync.RWMutex
}

// NewConfigMapEventHandler : create a new config map event hadler
func NewConfigMapEventHandler(state *State) *ConfigMapEventHandler {
	return &ConfigMapEventHandler{
		state: state,
	}
}

// AddToHandle : add event handler to framework handle
func (h *ConfigMapEventHandler) AddToHandle(handle framework.Handle) {
	h.Lock()
	defer h.Unlock()

	// filter config maps with label having prefix "sakkara."
	handle.SharedInformerFactory().Core().V1().ConfigMaps().Informer().AddEventHandler(
		clientcache.FilteringResourceEventHandler{
			// TODO: more efficient way to do the selection
			FilterFunc: func(obj interface{}) bool {
				switch obj := obj.(type) {
				case *v1.ConfigMap:
					{
						for k := range obj.GetLabels() {
							if strings.HasPrefix(k, KeyPrefix) {
								return true
							}
						}
					}
				default:
				}
				return false
			},
			Handler: h,
		},
	)
}

// OnAdd : add new group specs to group manager
func (h *ConfigMapEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	h.Lock()
	defer h.Unlock()

	cm := obj.(*v1.ConfigMap)
	klog.V(6).InfoS("ConfigMapEventHandler: OnAdd()", "cm", cm.GetNamespace()+"/"+cm.GetName())
	h.handleChange(cm)
}

// OnUpdate : update group specs in group manager
func (h *ConfigMapEventHandler) OnUpdate(oldObj, newObj interface{}) {
	h.Lock()
	defer h.Unlock()

	cm := newObj.(*v1.ConfigMap)
	klog.V(6).InfoS("ConfigMapEventHandler: OnUpdate()", "cm", cm.GetNamespace()+"/"+cm.GetName())
	h.handleChange(cm)
}

// OnDelete : delete group from group manager
func (h *ConfigMapEventHandler) OnDelete(obj interface{}) {
	h.Lock()
	defer h.Unlock()

	cm := obj.(*v1.ConfigMap)
	klog.V(6).InfoS("ConfigMapEventHandler: OnDelete()", "cm", cm.GetNamespace()+"/"+cm.GetName())
	h.handleDelete(cm)
}

// handleChange : update/create topology or group data in corresonding manager
func (h *ConfigMapEventHandler) handleChange(cm *v1.ConfigMap) {
	if _, isTopologyCM := cm.GetLabels()[TopologyConfigMapLabel]; isTopologyCM {
		if cm.GetNamespace() == TopologyConfigMapNameSpace {
			if topologyData, err := CreateTreeFromConfigMap(cm); err == nil {
				topologyManager.SetTopologyData(topologyData)
			}
		} else {
			klog.V(6).InfoS("handleChange: topology map not in configured namespace", "name", cm.GetName(),
				"nameSpace", cm.GetNamespace(), "configNameSpace", TopologyConfigMapNameSpace)
		}
		return
	}

	if _, isGroupCM := cm.GetLabels()[GroupConfigMapLabel]; isGroupCM {
		if groupData, err := CreateGroupFromConfigMap(cm); err == nil {
			currentGroupData := groupManager.GetGroupData(groupData.GroupKey())
			newGroupCM := currentGroupData == nil || currentGroupData.CMData.UID != groupData.CMData.UID
			groupManager.SetGroupData(groupData)
			// add to state and recover state
			groupKey := groupData.GroupKey()
			if _, exists := h.state.GetGroup(groupKey); !exists {
				group := NewGroup(groupData.Name, cm.GetNamespace())
				if h.state.AddGroup(group) {
					klog.V(6).InfoS("handleChange: new group", "ns", group.namespace, "name", group.name, "status", StatusName[group.status])
				} else {
					klog.V(6).InfoS("handleChange: failed to add group to state", "group", groupKey)
					return
				}

				if !newGroupCM {
					return
				}

				// recover state : status
				if status, statusExists := cm.Data[GroupConfigMapStatusKey]; statusExists {
					if index := slices.Index(StatusName, status); index >= 0 && index < len(StatusName) {
						group.status = Status(index)
					}
				}
				// recover state : delayed deletion
				if delayString, delayExists := cm.Data[GroupConfigMapDelayedDeletionKey]; delayExists {
					if delay, err := time.Parse(time.RFC3339, delayString); err == nil {
						group.delayedDeletionTime = &delay
					}
				}
				//recover state : preemptor
				groupKeyStr := groupKey.String()
				if preemptor, preemptorExists :=
					cm.Data[GroupConfigMapPreemptorKey]; preemptorExists && group.status == Preempted {
					h.state.preemptedGroups[groupKeyStr] = preemptor
					if _, groupExists := h.state.preemptingGroups[preemptor]; !groupExists {
						h.state.preemptingGroups[preemptor] = []string{groupKeyStr}
					} else {
						if !slices.Contains(h.state.preemptingGroups[preemptor], groupKeyStr) {
							h.state.preemptingGroups[preemptor] = append(h.state.preemptingGroups[preemptor], groupKeyStr)
						}
					}
				} else {
					if preemptingGroup, exists := h.state.preemptedGroups[groupKeyStr]; exists {
						delete(h.state.preemptedGroups, groupKeyStr)
						if _, preemptedExists := h.state.preemptingGroups[preemptingGroup]; preemptedExists {
							if index := slices.Index(h.state.preemptingGroups[preemptingGroup], groupKeyStr); index >= 0 {
								h.state.preemptingGroups[preemptingGroup] =
									slices.Delete(h.state.preemptingGroups[preemptingGroup], index, index)
							}
						}
					}
				}
			}
		}
		return
	}
}

// handleDelete : handleDelete topology or group data from corresonding manager
func (h *ConfigMapEventHandler) handleDelete(cm *v1.ConfigMap) {
	if _, isTopologyCM := cm.GetLabels()[TopologyConfigMapLabel]; isTopologyCM {
		if cm.GetNamespace() == TopologyConfigMapNameSpace {
			topologyManager.SetTopologyData(nil)
		} else {
			klog.V(6).InfoS("handleDelete: topology map not in configured namespace", "name", cm.GetName(),
				"nameSpace", cm.GetNamespace(), "configNameSpace", TopologyConfigMapNameSpace)
		}
		return
	}

	if _, isGroupCM := cm.GetLabels()[GroupConfigMapLabel]; isGroupCM {
		groupKey := CreateGroupKey(GetGroupNameFromConfigMap(cm), cm.GetNamespace())
		groupManager.DeleteGroupData(groupKey)
		h.state.UnsetPreempting(groupKey)
		return
	}
}
