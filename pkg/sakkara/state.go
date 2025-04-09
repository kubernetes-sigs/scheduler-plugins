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
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	v1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

// State : dynamic state of the groups and their members
type State struct {
	// all groups in the state (groupKeyStr -> Group)
	groups map[string]*Group

	// map from preempting groups to a list of victim (preempted) groups (groupKeyStr -> [groupKeyStr])
	preemptingGroups map[string][]string

	// map from preempted group to its preempting group (groupKeyStr -> groupKeyStr)
	preemptedGroups map[string]string

	// map of deleted pods
	deletedPods map[string]*PodTimedData

	// serialize operations on the state
	sync.RWMutex
}

// TODO: have separate status list for a group and a member
// TODO: do we need both?

// Status : status of group or member
type Status int

const (
	Init = iota
	Waiting
	Ready
	Allowed
	Assigned
	Scheduled
	Permitted
	Claimed
	Bound
	Preempting
	Preempted
	Failed
)

var StatusName = []string{
	"Init",
	"Waiting",
	"Ready",
	"Allowed",
	"Assigned",
	"Scheduled",
	"Permitted",
	"Claimed",
	"Bound",
	"Preempting",
	"Preempted",
	"Failed",
}

// Group : a placement group
type Group struct {
	name      string
	namespace string
	status    Status
	members   map[string]*Member

	delayedDeletionTime *time.Time
	delayedPods         map[string]*PodData

	// time first pod in group seen
	beginTime *time.Time
	// time group scheduling completed successfully
	endTime *time.Time
	// solver time in milli
	solverTime int64

	// serialize operations on a group
	sync.RWMutex
}

// Member : a member in a placement group
type Member struct {
	PodID        string
	PodName      string
	PodNameSpace string
	Status       Status
	Retries      int
	NodeName     *NodeName

	// serialize operations on a member
	sync.RWMutex
}

// NodeName : node name for various phases
type NodeName struct {
	Assigned  string
	Claimed   string
	Scheduled string
	Bound     string
}

// PodData : relevant data about a pod
type PodData struct {
	UID       types.UID
	Name      string
	NameSpace string
}

func NewPodData(pod *v1.Pod) *PodData {
	return &PodData{
		UID:       pod.GetUID(),
		Name:      pod.GetName(),
		NameSpace: pod.GetNamespace(),
	}
}

// PodTimedData : pod data and a time stamp
type PodTimedData struct {
	PodData *PodData
	Time    *time.Time
}

// NewState : create a new state
func NewState() *State {
	state := &State{
		groups:           make(map[string]*Group),
		preemptingGroups: make(map[string][]string),
		preemptedGroups:  make(map[string]string),
		deletedPods:      make(map[string]*PodTimedData),
	}
	// start periodic updates
	go func() {
		delayedGroupDeletionTicker := time.NewTicker(time.Second * DelayedGroupDeletionIntervalSeconds)
		for range delayedGroupDeletionTicker.C {
			deletedGroups := []string{}
			now := time.Now()
			state.Lock()
			for groupKeyStr, group := range state.groups {
				if group.delayedDeletionTime != nil && now.After(*group.delayedDeletionTime) {
					deletedGroups = append(deletedGroups, groupKeyStr)
					group.ResetDelayDeletion()
				}
			}
			if len(deletedGroups) > 0 {
				klog.V(6).InfoS("State: delayedGroupDeletionTicker", "numDeleted", len(deletedGroups))
			}
			state.Unlock()

			sort.Slice(deletedGroups, func(i, j int) bool {
				groupKeyi, _ := CreateGroupKeyFromString(deletedGroups[i])
				groupKeyj, _ := CreateGroupKeyFromString(deletedGroups[j])
				return groupManager.GetGroupPriority(groupKeyi) >
					groupManager.GetGroupPriority(groupKeyj)
			})

			for _, groupKeyStr := range deletedGroups {
				groupKey, _ := CreateGroupKeyFromString(groupKeyStr)
				group, found := state.GetGroup(groupKey)
				if !found {
					continue
				}
				delayedPods := group.GetDelayedPods()
				state.RemoveGroup(groupKey)
				for _, podData := range delayedPods {
					member := NewMember(podData)
					klog.V(6).InfoS("State: moving pod to activeQ", "memberName", member.PodName, "group", groupKey)
					member.Activate(Init)
				}
			}
		}
	}()

	// start periodic cleanup
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * CleanupIntervalSeconds)
		for range metricsUpdaterTicker.C {
			state.ClearSpace()

			// TODO: costly cleanup, find alternative
			//state.Cleanup()
		}
	}()

	return state
}

// GetGroup : get group by name from state
func (s *State) GetGroup(groupKey *GroupKey) (*Group, bool) {
	s.RLock()
	defer s.RUnlock()

	group, exists := s.groups[groupKey.String()]
	return group, exists
}

// AddGroup : add a group to the state
func (s *State) AddGroup(group *Group) bool {
	s.Lock()
	defer s.Unlock()

	groupKeyStr := group.KeyStr()
	if _, exists := s.groups[groupKeyStr]; !exists {
		s.groups[groupKeyStr] = group
		return true
	}
	return false
}

// RemoveGroup : remove a group from the state
func (s *State) RemoveGroup(groupKey *GroupKey) bool {
	s.Lock()
	defer s.Unlock()

	groupKeyStr := groupKey.String()
	if _, exists := s.groups[groupKeyStr]; exists {
		delete(s.groups, groupKeyStr)

		// cleanup preempting and preempted data structures
		if preemptedList, exist := s.preemptingGroups[groupKeyStr]; exist {
			for _, preempted := range preemptedList {
				if s.preemptedGroups[preempted] == groupKeyStr {
					delete(s.preemptedGroups, preempted)
				}
			}
			delete(s.preemptingGroups, groupKeyStr)
		}
		return true
	}
	return false
}

// GetGroupKeys : get group keys of all groups in the state
func (s *State) GetGroupKeys() []string {
	s.RLock()
	defer s.RUnlock()

	keys := make([]string, len(s.groups))
	i := 0
	for k := range s.groups {
		keys[i] = k
		i++
	}
	return keys
}

// AddDeletedPod :
func (s *State) AddDeletedPod(pod *v1.Pod, t *time.Time) {
	s.Lock()
	defer s.Unlock()

	s.deletedPods[string(pod.UID)] = &PodTimedData{
		PodData: &PodData{
			UID:       pod.GetUID(),
			Name:      pod.GetName(),
			NameSpace: pod.GetNamespace()},
		Time: t}
}

// RemoveDeletedPod :
func (s *State) RemoveDeletedPod(pod *v1.Pod) {
	s.Lock()
	defer s.Unlock()

	delete(s.deletedPods, string(pod.UID))
}

// isPodDeleted :
func (s *State) IsPodDeleted(pod *v1.Pod) bool {
	s.RLock()
	defer s.RUnlock()

	return s.deletedPods[string(pod.UID)] != nil
}

// ClearSpace: clear space due to stale deleted pods events
func (s *State) ClearSpace() {
	s.Lock()
	defer s.Unlock()

	for _, ptd := range s.deletedPods {
		if time.Since(*ptd.Time) >= time.Duration(PodDeletedKeepDurationSeconds*time.Second) {
			klog.V(6).InfoS("ClearSpace: deleted pod event stale, removing", "pod", ptd.PodData.Name, "podSpace", ptd.PodData.NameSpace)
			delete(s.deletedPods, string(ptd.PodData.UID))
		}
	}
}

// NewGroup : create a new group
func NewGroup(name string, namespace string) *Group {
	return &Group{
		name:                name,
		namespace:           namespace,
		status:              Init,
		members:             make(map[string]*Member),
		delayedDeletionTime: nil,
		delayedPods:         make(map[string]*PodData),
		beginTime:           nil,
		endTime:             nil,
	}
}

// GetGroupNameFromPod : extract group name from member pod
func GetGroupNameFromPod(pod *v1.Pod) (name string) {
	if name = pod.GetLabels()[GroupNameKey]; name == "" {
		name = pod.GetLabels()[v1alpha1.PodGroupLabel]
	}
	return name
}

// GetGroupSizeFromPod : extract group size from member pod (if labeled)
func GetGroupSizeFromPod(pod *v1.Pod) (size int) {
	size, _ = strconv.Atoi(pod.GetLabels()[GroupSizeKey])
	return size
}

// GetGroupPriorityFromPod : extract group priority from member pod (if labeled)
func GetGroupPriorityFromPod(pod *v1.Pod) (priority int) {
	priority, _ = strconv.Atoi(pod.GetLabels()[GroupPriorityKey])
	return priority
}

// AddMember : add member to group
func (group *Group) AddMember(member *Member) bool {
	group.Lock()
	defer group.Unlock()

	if _, exists := group.members[member.PodID]; !exists {
		group.members[member.PodID] = member
		return true
	}
	return false
}

// RemoveMember : remove member from group
func (group *Group) RemoveMember(member *Member) bool {
	group.Lock()
	defer group.Unlock()

	if _, exists := group.members[member.PodID]; exists {
		delete(group.members, member.PodID)
		return true
	}
	return false
}

// IsMember : is member in group
func (group *Group) IsMember(pod *v1.Pod) bool {
	group.RLock()
	defer group.RUnlock()

	_, memberExists := group.members[string(pod.GetUID())]
	return memberExists
}

// GetMember : get member, given by pod, from group
func (group *Group) GetMember(pod *v1.Pod) *Member {
	group.RLock()
	defer group.RUnlock()

	return group.members[string(pod.UID)]
}

// GetMembers : get a list of members in group
func (group *Group) GetMembers() []*Member {
	group.RLock()
	defer group.RUnlock()

	members := make([]*Member, len(group.members))
	i := 0
	for _, m := range group.members {
		members[i] = m
		i++
	}
	return members
}

// GetNumMembers : get current number of members in group
func (group *Group) GetNumMembers() int {
	group.RLock()
	defer group.RUnlock()

	return len(group.members)
}

// GetNumStatus : get number of members with a given status in group
func (group *Group) GetNumStatus(status Status) int {
	group.RLock()
	defer group.RUnlock()

	num := 0
	for _, m := range group.members {
		if m.Status == status {
			num++
		}
	}
	return num
}

// GetStatus : get status of group
func (group *Group) GetStatus() Status {
	group.RLock()
	defer group.RUnlock()

	return group.status
}

// SetStatus : set status of group
func (group *Group) SetStatus(status Status) {
	group.Lock()
	group.status = status
	group.Unlock()

	UpdateGroupStatus(group.GroupKey(), status)
}

// DelayDeletion :
func (group *Group) DelayDeletion() {
	now := time.Now()
	dl := now.Add(time.Duration(DelayedGroupDeletionDurationSeconds * time.Second))
	group.delayedDeletionTime = &dl

	UpdateGroupDelayedDeletion(group.GroupKey(), group.delayedDeletionTime)
}

// ResetDelayDeletion :
func (group *Group) ResetDelayDeletion() {
	group.Lock()
	defer group.Unlock()

	group.delayedDeletionTime = nil
	UpdateGroupDelayedDeletion(group.GroupKey(), group.delayedDeletionTime)
}

// AddDelayedPod :
func (group *Group) AddDelayedPod(pod *v1.Pod) {
	group.RLock()
	defer group.RUnlock()

	group.delayedPods[string(pod.UID)] = &PodData{
		UID:       pod.GetUID(),
		Name:      pod.GetName(),
		NameSpace: pod.GetNamespace()}
}

// AddDelayedPod :
func (group *Group) RemoveDelayedPod(podUID string) {
	group.Lock()
	defer group.Unlock()

	delete(group.delayedPods, podUID)
}

// GetDelayedPods :
func (group *Group) GetDelayedPods() []*PodData {
	group.RLock()
	defer group.RUnlock()

	podsData := make([]*PodData, len(group.delayedPods))
	i := 0
	for _, p := range group.delayedPods {
		podsData[i] = p
		i++
	}
	return podsData
}

// GroupKey : create a GroupKey object
func (group *Group) GroupKey() *GroupKey {
	return CreateGroupKey(group.name, group.namespace)
}

// KeyStr : make unique string for group
func (group *Group) KeyStr() string {
	return group.GroupKey().String()
}

// SetBeginTime :
func (group *Group) SetBeginTime(t *time.Time) {
	group.Lock()
	defer group.Unlock()

	group.beginTime = t
}

// GetBeginTime :
func (group *Group) GetBeginTime() *time.Time {
	group.RLock()
	defer group.RUnlock()

	return group.beginTime
}

// SetEndTime :
func (group *Group) SetEndTime(t *time.Time) {
	group.Lock()
	defer group.Unlock()

	group.endTime = t
}

// GetEndTime :
func (group *Group) GetEndTime() *time.Time {
	group.RLock()
	defer group.RUnlock()

	return group.endTime
}

// SetSolverTime :
func (group *Group) SetSolverTime(t int64) {
	group.Lock()
	defer group.Unlock()

	group.solverTime = t
}

// GetSolverTime :
func (group *Group) GetSolverTime() int64 {
	group.RLock()
	defer group.RUnlock()

	return group.solverTime
}

// NewMemberForPod : create a new Member
func NewMember(podData *PodData) *Member {
	return &Member{
		PodID:        string(podData.UID),
		PodName:      podData.Name,
		PodNameSpace: podData.NameSpace,
		Status:       Init,
		NodeName:     &NodeName{},
	}
}

// NewMemberForPod : create a new Member from pod
func NewMemberForPod(pod *v1.Pod) *Member {
	return NewMember(NewPodData(pod))
}

// SetStatus : set the member status and update member pod
func (member *Member) SetStatus(status Status) {
	member.Status = status

	member.UpdateMemberStatus(status)
}

// Activate : move member pod from unschedulable queue to active queue
func (member *Member) Activate(status Status) bool {
	member.Lock()
	defer member.Unlock()

	// update pod to cause pod to leave waiting queue
	// TODO: is there a more direct way?
	patch := []byte("{\"metadata\":{\"labels\":{" +
		"\"" + MemberStatusKey + "\":\"" + StatusName[status] + "\"" +
		",\"" + MemberRetriesKey + "\":\"" + strconv.Itoa(member.Retries) + "\"" +
		"}}}")
	//klog.V(6).InfoS("Patching pod:", "pod", member.Name, "patch", patch)
	_, err := handle.ClientSet().CoreV1().Pods(member.PodNameSpace).Patch(context.Background(), member.PodName, types.MergePatchType,
		patch, metav1.PatchOptions{})
	return err == nil
}

// UpdateMemberStatus : update status in member pod
func (member *Member) UpdateMemberStatus(status Status) bool {
	member.Lock()
	defer member.Unlock()

	// updating sequence retries causes pod to leave waiting queue
	patch := []byte("{\"metadata\":{\"labels\":{" +
		"\"" + MemberStatusKey + "\":\"" + StatusName[status] + "\"" +
		",\"" + MemberRetriesKey + "\":\"" + strconv.Itoa(member.Retries) + "\"" +
		"}}}")
	_, err := handle.ClientSet().CoreV1().Pods(member.PodNameSpace).Patch(context.Background(), member.PodName, types.MergePatchType,
		patch, metav1.PatchOptions{})
	return err == nil
}

// Cleanup : cleanup all groups in the state
func (s *State) Cleanup() {
	keys := s.GetGroupKeys()
	for _, k := range keys {
		if groupKey, err := CreateGroupKeyFromString(k); err != nil {
			s.CleanupGroup(groupKey)
		}
	}
}

// CleanupGroup : delete group members that correspond to nonexisting pods
func (s *State) CleanupGroup(groupKey *GroupKey) {
	// klog.V(6).InfoS("Cleanup: cleaning begins", "group", groupKey)
	if group, exists := s.groups[groupKey.String()]; exists {
		group.Lock()

		// check if members pods deleted
		deletedMembers := make([]string, 0)
		for _, m := range group.members {
			podName := m.PodName
			podSpace := m.PodNameSpace
			pi := handle.ClientSet().CoreV1().Pods(podSpace)
			p, err := pi.Get(context.Background(), podName, metav1.GetOptions{})
			if p == nil || err != nil {
				klog.V(6).InfoS("Cleanup: pod unknown; removing from state", "pod", podName, "podSpace", podSpace)
				deletedMembers = append(deletedMembers, m.PodID)
			}
		}
		// update group members
		for _, mID := range deletedMembers {
			delete(group.members, mID)
		}
		if len(group.members) == 0 {
			if group.status == Preempted {
				group.DelayDeletion()
			} else {
				s.RemoveGroup(groupKey)
			}
		}
		group.Unlock()
	}
	// klog.V(6).InfoS("Cleanup: cleaning ends ", "group", groupKey)
}

// SetPreempting :
func (s *State) SetPreempting(preemptingGroupKey *GroupKey, preemptedGroupKeysStr []string) {
	s.Lock()
	defer s.Unlock()

	preemptingGroupKeyStr := preemptingGroupKey.String()
	s.preemptingGroups[preemptingGroupKeyStr] = preemptedGroupKeysStr
	if preemptingGroup, exists := s.groups[preemptingGroupKeyStr]; exists {
		preemptingGroup.status = Preempting
		UpdateGroupStatus(preemptingGroupKey, Preempting)
		for _, member := range preemptingGroup.members {
			member.SetStatus(Preempting)
			klog.V(6).InfoS("SetPreempting: member marked Preempting", "memberName", member.PodName, "preemptingGroup", preemptingGroupKey)

			if wp := handle.GetWaitingPod(types.UID(member.PodID)); wp != nil {
				klog.V(6).InfoS("SetPreempting: unblocking pod from permit waiting area", "pod", member.PodName)
				wp.Reject(Name, "unblocking preempting pod from permit waiting area")
			}
		}
	}

	for _, preempted := range preemptedGroupKeysStr {
		s.preemptedGroups[preempted] = preemptingGroupKeyStr
		if preemptedGroup, exists := s.groups[preempted]; exists {
			preemptedGroup.status = Preempted
			preemptedGroupKey, _ := CreateGroupKeyFromString(preempted)
			UpdateGroupStatus(preemptedGroupKey, Preempted)
			UpdateGroupPreemptor(preemptedGroupKey, preemptingGroupKeyStr)
			for _, member := range preemptedGroup.members {
				member.SetStatus(Preempted)
				klog.V(6).InfoS("SetPreempting: member marked Preempted", "memberName", member.PodName, "preemptedGroup", preempted)
			}
		}
	}
}

// UnsetPreempting :
func (s *State) UnsetPreempting(preemptingGroupKey *GroupKey) {
	s.Lock()
	defer s.Unlock()

	preemptingGroupKeyStr := preemptingGroupKey.String()
	for _, preempted := range s.preemptingGroups[preemptingGroupKeyStr] {
		preemptedKey, _ := CreateGroupKeyFromString(preempted)
		UpdateGroupPreemptor(preemptedKey, "")
		delete(s.preemptedGroups, preempted)
	}
	delete(s.preemptingGroups, preemptingGroupKeyStr)
}

// IsPreempting :
func (s *State) IsPreempting(preemptingGroupKeyStr string) bool {
	s.RLock()
	defer s.RUnlock()

	_, exists := s.preemptingGroups[preemptingGroupKeyStr]
	return exists
}

// GetPreemptedGroups :
func (s *State) GetPreemptedGroups(preemptingGroupKeyStr string) []string {
	s.RLock()
	defer s.RUnlock()

	return s.preemptingGroups[preemptingGroupKeyStr]
}

// GetPreemptingGroup :
func (s *State) GetPreemptingGroup(preemptedGroupKeyStr string) string {
	s.RLock()
	defer s.RUnlock()

	return s.preemptedGroups[preemptedGroupKeyStr]
}

// IsPreemptedGroup :
func (s *State) IsPreemptedGroup(preemptedGroupKeyStr string) bool {
	s.RLock()
	defer s.RUnlock()

	return len(s.preemptedGroups[preemptedGroupKeyStr]) > 0
}
