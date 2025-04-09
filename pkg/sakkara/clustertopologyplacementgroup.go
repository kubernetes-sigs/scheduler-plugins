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

// Package sakkara plugin schedules placement groups of homogeneous pods with hierarchical
// topology constraints.

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	pluginConfig "sigs.k8s.io/scheduler-plugins/apis/config"
	v1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"

	clientscheme "k8s.io/client-go/kubernetes/scheme"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

var _ framework.PreEnqueuePlugin = &ClusterTopologyPlacementGroup{}
var _ framework.QueueSortPlugin = &ClusterTopologyPlacementGroup{}
var _ framework.PostFilterPlugin = &ClusterTopologyPlacementGroup{}
var _ framework.PreScorePlugin = &ClusterTopologyPlacementGroup{}
var _ framework.ScorePlugin = &ClusterTopologyPlacementGroup{}
var _ framework.ScoreExtensions = &ClusterTopologyPlacementGroup{}
var _ framework.ReservePlugin = &ClusterTopologyPlacementGroup{}
var _ framework.PermitPlugin = &ClusterTopologyPlacementGroup{}
var _ framework.PostBindPlugin = &ClusterTopologyPlacementGroup{}

const (
	// name of plugin
	Name = "ClusterTopologyPlacementGroup"
)

var (
	// the topology manager
	topologyManager *TopologyManager
	// the group manager
	groupManager *GroupManager
	// the framework handle
	handle framework.Handle
	// the namespace where the topology tree configmap is deployed
	TopologyConfigMapNameSpace string
	// client to access pod groups
	podGroupClient client.Client
)

// ClusterTopologyPlacementGroup : scheduler plugin
type ClusterTopologyPlacementGroup struct {
	args  *pluginConfig.ClusterTopologyPlacementGroupArgs
	state *State

	// for serialize operations
	sync.RWMutex
}

// New : create an instance of a ClusterTopologyPlacementGroup plugin
func New(ctx context.Context, obj runtime.Object, frameworkHandle framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Creating new instance of the ClusterTopologyPlacementGroup plugin")
	// cast object into plugin arguments object
	args, ok := obj.(*pluginConfig.ClusterTopologyPlacementGroupArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type ClusterTopologyPlacementGroupArgs, got %T", obj)
	}

	TopologyConfigMapNameSpace = args.TopologyConfigMapNameSpace
	logger.V(4).Info("Using ClusterTopologyPlacementGroupArgs", "TopologyConfigMapNameSpace", TopologyConfigMapNameSpace)

	topologyManager = NewTopologyManager()
	groupManager = NewGroupManager()
	handle = frameworkHandle

	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	//_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	if client, err := client.New(handle.KubeConfig(), client.Options{Scheme: scheme}); err == nil {
		podGroupClient = client
	} else {
		return nil, err
	}

	pl := &ClusterTopologyPlacementGroup{
		args:  args,
		state: NewState(),
	}

	// create event handlers
	cmEventHandler := NewConfigMapEventHandler(pl.state)
	cmEventHandler.AddToHandle(handle)
	podEventHandler := NewPodEventHandler(pl.state)
	podEventHandler.AddToHandle(handle)

	return pl, nil
}

// Name : name of plugin
func (pl *ClusterTopologyPlacementGroup) Name() string {
	return Name
}

// PreEnqueue : called prior to adding Pods to activeQ
func (pl *ClusterTopologyPlacementGroup) PreEnqueue(ctx context.Context, p *v1.Pod) *framework.Status {
	logger := klog.FromContext(ctx)
	podName := p.GetName()
	nameSpace := p.GetNamespace()
	groupName := GetGroupNameFromPod(p)
	groupKey := CreateGroupKey(groupName, nameSpace)

	logger.V(6).Info("PreEnqueue: pod arrival", "podName", podName, "nameSpace", nameSpace, "groupName", groupName)

	// check if zombie pod
	if pl.state.IsPodDeleted(p) {
		logger.V(6).Info("PreEnqueue: zombie pod, already deleted, skipping", "podName", podName)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s in group %s already deleted, skipping", podName, groupKey))
	}

	// check if pod group is known to group manager (has seen cm for the group)
	groupData := groupManager.GetGroupData(groupKey)
	if groupData == nil {

		logger.V(6).Info("PreEnqueue: missing group data", "podName", podName)
		// check if a corresponding PodGroup exists
		var pg v1alpha1.PodGroup
		if err := podGroupClient.Get(ctx, types.NamespacedName{Namespace: nameSpace, Name: groupName}, &pg); err != nil {
			logger.V(6).Info("PreEnqueue: no podgroup or configmap found", "podName", podName)
			return framework.NewStatus(framework.Unschedulable,
				fmt.Sprintf("Pod %v missing placement group data", podName))
		}

		logger.V(6).Info("PreEnqueue: found podgroup", "name", pg.GetName())
		groupSize := pg.Spec.MinMember
		groupPriority := p.Spec.Priority
		groupData = &GroupData{
			Name:      groupName,
			NameSpace: nameSpace,
			Size:      int(groupSize),
			Priority:  int(*groupPriority),
			CMData:    nil,
		}
		logger.V(6).Info("PreEnqueue: created new group data", "groupData", groupData)
		groupManager.SetGroupData(groupData)
	}
	groupSize := groupData.Size

	// add group to state if does not exist
	group, groupExists := pl.state.GetGroup(groupKey)
	if !groupExists {
		logger.V(6).Info("PreEnqueue: pod first in group, adding group", "ns", nameSpace, "groupName", groupName)
		group = NewGroup(groupName, nameSpace)
		pl.state.AddGroup(group)
		group.SetStatus(Waiting)
	}

	numMembers := group.GetNumMembers()
	groupStatus := group.GetStatus()
	logger.V(6).Info("PreEnqueue: pod group exists", "podName", podName, "ns", nameSpace, "groupName", groupName,
		"groupSize", groupSize, "groupStatus", StatusName[groupStatus])

	// check if pod belongs to a preempted group, i.e. new pod as replacement to a deleted pod, then keep it pending,
	// until all pods in preempted group are deleted and preempted group is deleted from the system
	if groupStatus == Preempted {
		logger.V(6).Info("PreEnqueue: group status Preempted", "podName", podName)
		group.AddDelayedPod(p)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %v group status Preempted", podName))
	}

	// check if pod member beyond group size
	if !group.IsMember(p) && numMembers >= groupSize {
		logger.V(6).Info("PreEnqueue: pod beyond group size, rejecting", "podName", podName, "ns", nameSpace,
			"groupName", groupName, "numMembers", numMembers, "groupSize", groupSize)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s beyond size of placement group %s", podName, groupKey))
	}

	// add pod member to group, if does not exist
	if added := group.AddMember(NewMemberForPod(p)); added {
		logger.V(6).Info("PreEnqueue: added pod as member in group", "podName", podName, "group", groupKey)
		// timestamp start time
		if numMembers == 0 {
			now := time.Now()
			group.SetBeginTime(&now)
		}
	}
	member := group.GetMember(p)
	logger.V(6).Info("PreEnqueue: member in group", "podName", podName, "ns", nameSpace, "groupName", groupName,
		"memberStatus", StatusName[member.Status])

	// set member status to Waiting if new or preempting or failed the scheduling cycle
	if member.Status == Init || member.Status == Preempting || member.Status == Permitted || group.GetStatus() == Failed {
		logger.V(6).Info("PreEnqueue: setting member status to Waiting", "podName", podName, "group", groupKey)
		member.SetStatus(Waiting)
	}

	numMembers = group.GetNumMembers()
	numWaiting := group.GetNumStatus(Waiting)
	memberStatus := member.Status
	logger.V(6).Info("PreEnqueue: member in group", "podName", podName, "ns", nameSpace, "groupName", groupName,
		"memberStatus", StatusName[memberStatus], "groupSize", groupSize, "numMembers", numMembers, "numWaiting", numWaiting)

	// take action based on member state machine
	switch memberStatus {
	case Waiting:
		if numWaiting >= groupSize {
			logger.V(6).Info("PreEnqueue: last member in group, ready for activation", "podName", podName, "group", groupKey)
			// group ready to be scheduled
			group.SetStatus(Ready)
			members := group.GetMembers()
			count := min(groupSize, len(members))
			for i := 0; i < count; i++ {
				member := members[i]
				member.SetStatus(Ready)
				member.Retries++
				logger.V(6).Info("PreEnqueue: member marked ready", "memberName", member.PodName, "group", groupKey,
					"memberStatus", StatusName[member.Status])
				// TODO: use framework Activate()
				if !member.Activate(Ready) {
					return framework.NewStatus(framework.Error,
						fmt.Sprintf("Failed to activate (by patching) pod %s in group %s", podName, groupKey))
				}
			}
		}

	case Ready, Assigned:
		return framework.NewStatus(framework.Success,
			fmt.Sprintf("Pod %s in group %s ready for scheduling", podName, groupKey))

	// TODO:
	default:

	}

	// TODO: handle all cases

	// keep pod in the unschedulable pods queue
	return framework.NewStatus(framework.Unschedulable,
		fmt.Sprintf("Pod %s waiting for remaining pods in group %s", podName, groupKey))
}

// Less : used to sort pods in the scheduling queue
func (pl *ClusterTopologyPlacementGroup) Less(podInfo1, podInfo2 *framework.QueuedPodInfo) bool {
	group1 := podInfo1.Pod.GetLabels()[GroupNameKey]
	group2 := podInfo2.Pod.GetLabels()[GroupNameKey]
	if group1 == group2 {
		t1 := podInfo1.Timestamp
		t2 := podInfo2.Timestamp
		return t1.Before(t2)
	}

	groupPriority1 := groupManager.GetGroupPriority(CreateGroupKey(group1, podInfo1.Pod.Namespace))
	groupPriority2 := groupManager.GetGroupPriority(CreateGroupKey(group2, podInfo2.Pod.Namespace))
	if groupPriority1 == groupPriority2 {
		return group1 < group2
	}

	return groupPriority1 > groupPriority2
}

// PostFilter : called by the scheduling framework after the filtering phase if a pod cannot be scheduled
func (pl *ClusterTopologyPlacementGroup) PostFilter(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {

	logger := klog.FromContext(ctx)
	podName := pod.GetName()
	nameSpace := pod.GetNamespace()
	groupName := GetGroupNameFromPod(pod)
	groupKey := CreateGroupKey(groupName, nameSpace)

	// check if zombie pod
	if pl.state.IsPodDeleted(pod) {
		logger.V(6).Info("PostFilter: zombie pod, already deleted, skipping", "podName", podName)
		return nil, framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s in group %s already deleted, skipping", podName, groupKey))
	}

	group, groupExists := pl.state.GetGroup(groupKey)

	if !groupExists || group == nil {
		logger.V(6).Info("PostFilter: pod member of unknown group, skipping", "podName", podName)
		return nil, framework.NewStatus(framework.Unschedulable, "done PostFilter")
	}

	member := group.GetMember(pod)
	if member == nil {
		logger.V(6).Info("PostFilter: pod not member of group", "podName", podName, "groupName", groupName)
		return nil, framework.NewStatus(framework.Unschedulable, "done PostFilter")
	}
	if member.Status == Preempting {
		logger.V(6).Info("PostFilter: pod already marked Preempting", "podName", podName, "group", groupKey)
		return nil, framework.NewStatus(framework.Unschedulable, "done PostFilter")
	}

	logger.V(6).Info("PostFilter: attempting preemption ...")
	if group.GetStatus() == Preempting {
		logger.V(6).Info("PostFilter: group has already preempted", "podName", podName, "group", groupKey)
		return nil, framework.NewStatus(framework.Unschedulable, "done PostFilter")
	}
	if err := pl.attemptPreemption(pod, groupKey, logger); err != nil {
		logger.V(6).Info("PostFilter: attempt preemption failed", "err", err)
	}
	return nil, framework.NewStatus(framework.Unschedulable, "done PostFilter")
}

// PreScore : called by the scheduling framework after a list of nodes passed the filtering phase
func (pl *ClusterTopologyPlacementGroup) PreScore(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod, nodeInfos []*framework.NodeInfo) *framework.Status {

	logger := klog.FromContext(ctx)
	podName := pod.GetName()
	nameSpace := pod.GetNamespace()
	groupName := GetGroupNameFromPod(pod)
	groupKey := CreateGroupKey(groupName, nameSpace)

	// check if zombie pod
	if pl.state.IsPodDeleted(pod) {
		logger.V(6).Info("PreScore: zombie pod, already deleted, skipping", "podName", podName)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s in group %s already deleted, skipping", podName, groupKey))
	}

	group, groupExists := pl.state.GetGroup(groupKey)
	logger.V(6).Info("PreScore: pod arrival", "podName", podName, "group", groupKey)

	// check if pod not in a group
	if !groupExists || group == nil {
		logger.V(6).Info("PreScore: pod member of unknown group", "podName", podName)
		return nil
	}

	member := group.GetMember(pod)
	if member == nil {
		logger.V(6).Info("PreScore: pod not member of group", "podName", podName, "group", groupKey)
		return nil
	}
	if member.Status == Assigned {
		logger.V(6).Info("PreScore: pod already assigned a node name", "podName", podName,
			"groupName", groupName, "nodeNameAssigned", member.NodeName.Assigned)
		return nil
	}
	if member.Status == Preempting {
		logger.V(6).Info("PreScore: pod already marked Preempting", "podName", podName, "group", groupKey)
		return framework.NewStatus(framework.Unschedulable, "Retry scheduling of preempting pod")
	}
	if group.GetStatus() == Failed {
		logger.V(6).Info("PreScore: group status Failed, no attempt to place group")
		member.SetStatus(Failed)
		return framework.NewStatus(framework.Unschedulable, "Group in Failed state")
	}

	logger.V(6).Info("PreScore: solving the group placement problem, triggered by", "podName", podName)

	// solve topology-aware placement group problem
	startTime := time.Now()
	solver, err := NewSolver()
	if err != nil {
		logger.V(6).Error(err, "Solver creation failed")
		return framework.NewStatus(framework.Error, "Solver creation failed")
	}

	nodes := make([]*v1.Node, len(nodeInfos))
	for k, ni := range nodeInfos {
		nodes[k] = ni.Node()
	}
	err = solver.CreateProblemInstance(pod, nodes)
	if err != nil {
		logger.V(6).Error(err, "Creating problem instance failed")
		return framework.NewStatus(framework.Error, "Creating problem instance failed")
	}
	nodeNames, err := solver.Solve()
	endTime := time.Now()
	group.SetSolverTime(endTime.Sub(startTime).Milliseconds())
	if err != nil {
		group.SetStatus(Failed)
		logger.V(6).Info("PreScore: solver failed to assign nodes, attempting preemption ...")
		if err := pl.attemptPreemption(pod, groupKey, logger); err != nil {
			logger.V(6).Info("PreScore: attempt preemption failed", "err", err)
		}
		return framework.NewStatus(framework.Unschedulable, "Solver failed to assign nodes")
	}
	group.SetStatus(Assigned)
	members := group.GetMembers()
	count := min(len(nodeNames), len(members))
	for i := 0; i < count; i++ {
		member := members[i]
		member.NodeName.Assigned = nodeNames[i]
		member.SetStatus(Assigned)
		logger.V(6).Info("PreScore: member assigned a node name", "memberName", member.PodName,
			"ns", nameSpace, "groupName", groupName, "nodeNameAssigned", member.NodeName.Assigned)
	}
	return nil
}

// Score : called on each filtered node
func (pl *ClusterTopologyPlacementGroup) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	logger := klog.FromContext(ctx)
	logger.V(6).Info("Score: Calculating score", "pod", klog.KObj(pod), "nodeName", nodeName)

	score := framework.MinNodeScore
	podName := pod.GetName()
	nameSpace := pod.GetNamespace()
	groupName := GetGroupNameFromPod(pod)
	groupKey := CreateGroupKey(groupName, nameSpace)

	// check if zombie pod
	if pl.state.IsPodDeleted(pod) {
		logger.V(6).Info("Score: zombie pod, already deleted, skipping", "podName", podName)
		return score, framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s in group %s already deleted, skipping", podName, groupKey))
	}

	group, groupExists := pl.state.GetGroup(groupKey)

	if !groupExists || group == nil {
		logger.V(6).Info("Score: pod member of unknown group", "podName", podName, "nodeName", nodeName, "score", score)
		return score, nil
	}

	member := group.GetMember(pod)
	if member == nil {
		logger.V(6).Info("Score: pod not member of group", "podName", podName, "group", groupKey,
			"nodeName", nodeName, "score", score)
		return score, nil
	}
	if member.Status == Assigned {
		if member.NodeName.Assigned == nodeName {
			score = framework.MaxNodeScore
		}
		logger.V(6).Info("Score: pod is assigned a node name", "podName", podName, "group", groupKey,
			"nodeNameAssigned", member.NodeName.Assigned, "nodeName", nodeName, "score", score)
		return score, framework.NewStatus(framework.Success, "")
	}
	logger.V(6).Info("Score: pod not assigned a node name", "podName", podName, "group", groupKey,
		"nodeName", nodeName, "score", score, "memberStatus", StatusName[member.Status])
	return score, nil
}

// ScoreExtensions : an interface for Score extended functionality
func (pl *ClusterTopologyPlacementGroup) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

// NormalizeScore : normalize scores
func (pl *ClusterTopologyPlacementGroup) NormalizeScore(context.Context, *framework.CycleState,
	*v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// Reserve : update pluging state given that the scheduler cache is updated
func (pl *ClusterTopologyPlacementGroup) Reserve(ctx context.Context, state *framework.CycleState,
	p *v1.Pod, nodeName string) *framework.Status {

	logger := klog.FromContext(ctx)
	podName := p.GetName()
	nameSpace := p.GetNamespace()
	groupName := GetGroupNameFromPod(p)
	groupKey := CreateGroupKey(groupName, nameSpace)

	// check if zombie pod
	if pl.state.IsPodDeleted(p) {
		logger.V(6).Info("Reserve: zombie pod, already deleted, skipping", "podName", podName)
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("Pod %s in group %s already deleted, skipping", podName, groupKey))
	}

	logger.V(6).Info("Reserve:", "pod", klog.KObj(p), "nodeName", nodeName, "group", groupKey)
	group, groupExists := pl.state.GetGroup(groupKey)
	if groupExists && (group.GetStatus() == Assigned || groupManager.GetGroupSize(groupKey) == 1) {
		if member := group.GetMember(p); member != nil {
			member.SetStatus(Scheduled)
			member.NodeName.Scheduled = nodeName
			logger.V(6).Info("Reserve: pod scheduled", "podName", podName, "ns", nameSpace, "groupName", groupName,
				"nodeNameAssigned", member.NodeName.Assigned, "nodeNameScheduled", member.NodeName.Scheduled)
			return framework.NewStatus(framework.Success, "")
		}
	}
	return framework.NewStatus(framework.Unschedulable, "unsuccessful Reserve: unknown group/member or unassigned group")
}

// Unreserve : update pluging state given that assumed pod to node failed
func (pl *ClusterTopologyPlacementGroup) Unreserve(ctx context.Context, state *framework.CycleState,
	p *v1.Pod, nodeName string) {

	logger := klog.FromContext(ctx)
	podName := p.GetName()
	nameSpace := p.GetNamespace()
	groupName := GetGroupNameFromPod(p)
	groupKey := CreateGroupKey(groupName, nameSpace)
	group, groupExists := pl.state.GetGroup(groupKey)
	if groupExists {
		if member := group.GetMember(p); member != nil {
			member.NodeName.Scheduled = ""
			member.SetStatus(Waiting)
			logger.V(6).Info("Unreserve: pod scheduled", "podName", podName, "group", groupKey,
				"nodeNameAssigned", member.NodeName.Assigned, "nodeNameScheduled", member.NodeName.Scheduled)
		}
	}
}

// Permit : used to prevent or delay the binding of a Pod
func (pl *ClusterTopologyPlacementGroup) Permit(ctx context.Context, state *framework.CycleState,
	p *v1.Pod, nodeName string) (*framework.Status, time.Duration) {

	logger := klog.FromContext(ctx)
	podName := p.GetName()

	// check if zombie pod
	if pl.state.IsPodDeleted(p) {
		logger.V(6).Info("Permit: zombie pod, already deleted, skipping", "podName", podName)
		return framework.NewStatus(framework.Success, ""), 0
	}

	nameSpace := p.GetNamespace()
	groupName := GetGroupNameFromPod(p)
	groupKey := CreateGroupKey(groupName, nameSpace)
	group, groupExists := pl.state.GetGroup(groupKey)
	if groupExists {
		if member := group.GetMember(p); member != nil {

			if member.Status == Preempting {
				logger.V(6).Info("Permit: pod already marked Preempting", "podName", podName, "group", groupKey)
				return framework.NewStatus(framework.Unschedulable, "Retry scheduling of preempting pod"), 0
			}

			member.SetStatus(Permitted)
			numPermitted := group.GetNumStatus(Permitted)
			groupSize := groupManager.GetGroupSize(groupKey)
			logger.V(6).Info("Permit: ", "podName", podName, "group", groupKey,
				"numPermitted", numPermitted, "groupSize", groupSize)
			if numPermitted >= groupSize {
				group.SetStatus(Permitted)
				for _, member := range group.GetMembers() {
					if wp := handle.GetWaitingPod(types.UID(member.PodID)); wp != nil {
						logger.V(6).Info("Permit: allowing", "pod", member.PodName)
						wp.Allow(pl.Name())
					}
				}
			} else {
				return framework.NewStatus(framework.Wait, ""), time.Minute
			}
		}
	}
	return framework.NewStatus(framework.Success, ""), 0
}

// PostBind : PostBind is called after a pod is successfully bound
func (pl *ClusterTopologyPlacementGroup) PostBind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {

	pl.Lock()
	defer pl.Unlock()

	logger := klog.FromContext(ctx)
	podName := p.GetName()

	// check if zombie pod
	if pl.state.IsPodDeleted(p) {
		logger.V(6).Info("PostBind: zombie pod, already deleted, skipping", "podName", podName)
		return
	}

	nameSpace := p.GetNamespace()
	groupName := GetGroupNameFromPod(p)
	groupKey := CreateGroupKey(groupName, nameSpace)
	group, groupExists := pl.state.GetGroup(groupKey)
	if groupExists {
		if member := group.GetMember(p); member != nil {
			member.SetStatus(Bound)
			member.NodeName.Bound = nodeName
			numBound := group.GetNumStatus(Bound)
			groupSize := groupManager.GetGroupSize(groupKey)
			logger.V(6).Info("PostBind: ", "podName", podName, "group", groupKey,
				"numBound", numBound, "groupSize", groupSize)
			if numBound >= groupSize {
				group.SetStatus(Bound)
				nodeNamesToPodNames := make(map[string][]string)
				podsMap := make(map[string]*PodData)
				for _, member := range group.GetMembers() {
					if member.Status != Bound {
						continue
					}
					nodeName := member.NodeName.Bound
					podName := member.PodName
					nodeNamesToPodNames[nodeName] = append(nodeNamesToPodNames[nodeName], podName)
					podsMap[podName] = &PodData{
						UID:       types.UID(member.PodID),
						Name:      podName,
						NameSpace: member.PodNameSpace,
					}
				}
				bTree := topologyManager.GetBoundTree(nodeNamesToPodNames)
				if bTree == nil {
					logger.V(6).Info("PostBind: bound tree is nil")
					return
				}
				pl.state.UnsetPreempting(groupKey)

				// set end timestamp
				now := time.Now()
				group.SetEndTime(&now)

				UpdateGroupConfigMap(groupKey, group, bTree, podsMap)
			}
		}
	}
}

// attemptPreemption:
func (pl *ClusterTopologyPlacementGroup) attemptPreemption(pod *v1.Pod, groupKey *GroupKey, logger klog.Logger) (err error) {
	preemptingPriority := groupManager.GetGroupPriority(groupKey)
	var preemptor *Preemptor
	preemptor, err = NewPreemptor(preemptingPriority)
	if err != nil {
		return fmt.Errorf("NewPreemptor: failed, err=%s", err.Error())
	}
	if err = preemptor.Initialize(); err != nil {
		return fmt.Errorf("Preemptor.Initialize: failed, err=%s", err.Error())
	}
	logger.V(6).Info("Preemptor: ", "preemptor", preemptor)
	var victimGroupNames []string
	if victimGroupNames, err = preemptor.Preempt(pod); err != nil {
		return fmt.Errorf("Preemptor.Preempt: failed, err=%s", err.Error())
	}
	pl.state.SetPreempting(groupKey, victimGroupNames)
	preemptor.DeleteVictimGroups()
	return nil
}
