/*
Copyright 2022 The Kubernetes Authors.

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

package util

import (
	v1 "k8s.io/api/core/v1"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
)

// CostKey : key for map concerning network costs (origin / destinations)
type CostKey struct {
	Origin      string
	Destination string
}

// ScheduledInfo : struct for scheduled pods
type ScheduledInfo struct {
	// Pod Name
	Name string

	// Pod AppGroup Selector
	Selector string

	// Replica ID
	ReplicaID string

	// Hostname
	Hostname string
}

type ScheduledList []ScheduledInfo

// GetNodeRegion : return the region of the node
func GetNodeRegion(node *v1.Node) string {
	labels := node.Labels
	if labels == nil {
		return ""
	}
	return labels[v1.LabelTopologyRegion]
}

// GetNodeZone : return the zone of the node
func GetNodeZone(node *v1.Node) string {
	labels := node.Labels
	if labels == nil {
		return ""
	}
	return labels[v1.LabelTopologyZone]
}

// GetPodAppGroupLabel : get AppGroup from pod annotations
func GetPodAppGroupLabel(pod *v1.Pod) string {
	return pod.Labels[agv1alpha1.AppGroupLabel]
}

// GetPodAppGroupSelector : get Workload Selector from pod annotations
func GetPodAppGroupSelector(pod *v1.Pod) string {
	return pod.Labels[agv1alpha1.AppGroupSelectorLabel]
}

// ByTopologyKey : Sort TopologyList by TopologyKey
type ByTopologyKey ntv1alpha1.TopologyList

func (s ByTopologyKey) Len() int {
	return len(s)
}

func (s ByTopologyKey) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByTopologyKey) Less(i, j int) bool {
	return s[i].TopologyKey < s[j].TopologyKey
}

// ByOrigin : Sort OriginList by Origin (e.g., Region Name, Zone Name)
type ByOrigin ntv1alpha1.OriginList

func (s ByOrigin) Len() int {
	return len(s)
}

func (s ByOrigin) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByOrigin) Less(i, j int) bool {
	return s[i].Origin < s[j].Origin
}

// ByDestination : Sort CostList by Destination (e.g., Region Name, Zone Name)
type ByDestination ntv1alpha1.CostList

func (s ByDestination) Len() int {
	return len(s)
}

func (s ByDestination) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByDestination) Less(i, j int) bool {
	return s[i].Destination < s[j].Destination
}

// ByWorkloadSelector : Sort AppGroupTopologyList by Workload.Selector
type ByWorkloadSelector agv1alpha1.AppGroupTopologyList

func (s ByWorkloadSelector) Len() int {
	return len(s)
}

func (s ByWorkloadSelector) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByWorkloadSelector) Less(i, j int) bool {
	return s[i].Workload.Selector < s[j].Workload.Selector
}

// FindPodOrder : return the order index of the given pod
func FindPodOrder(t agv1alpha1.AppGroupTopologyList, selector string) int32 {
	low := 0
	high := len(t) - 1

	for low <= high {
		mid := (low + high) / 2
		if t[mid].Workload.Selector == selector {
			return t[mid].Index // Return the index
		} else if t[mid].Workload.Selector < selector {
			low = mid + 1
		} else if t[mid].Workload.Selector > selector {
			high = mid - 1
		}
	}
	return -1
}

// FindOriginCosts : return the costList for a certain origin
func FindOriginCosts(originList []ntv1alpha1.OriginInfo, origin string) []ntv1alpha1.CostInfo {
	low := 0
	high := len(originList) - 1

	for low <= high {
		mid := (low + high) / 2
		if originList[mid].Origin == origin {
			return originList[mid].CostList // Return the CostList
		} else if originList[mid].Origin < origin {
			low = mid + 1
		} else if originList[mid].Origin > origin {
			high = mid - 1
		}
	}
	// Costs were not found
	return []ntv1alpha1.CostInfo{}
}

// FindTopologyKey : return the originList for a certain key
func FindTopologyKey(topologyList []ntv1alpha1.TopologyInfo, key ntv1alpha1.TopologyKey) ntv1alpha1.OriginList {
	low := 0
	high := len(topologyList) - 1

	for low <= high {
		mid := (low + high) / 2
		if topologyList[mid].TopologyKey == key {
			return topologyList[mid].OriginList // Return the OriginList
		} else if topologyList[mid].TopologyKey < key {
			low = mid + 1
		} else if topologyList[mid].TopologyKey > key {
			high = mid - 1
		}
	}
	// Topology Key was not found
	return ntv1alpha1.OriginList{}
}

// GetDependencyList : get workload dependencies established in the AppGroup CR
func GetDependencyList(pod *v1.Pod, ag *agv1alpha1.AppGroup) []agv1alpha1.DependenciesInfo {

	// Check Dependencies of the given pod
	var dependencyList []agv1alpha1.DependenciesInfo

	// Get Labels of the given pod
	podLabels := pod.GetLabels()

	for _, w := range ag.Spec.Workloads {
		if w.Workload.Selector == podLabels[agv1alpha1.AppGroupSelectorLabel] {
			for _, dependency := range w.Dependencies {
				dependencyList = append(dependencyList, dependency)
			}
		}
	}

	// Return the dependencyList
	return dependencyList
}

// GetScheduledList : get Pods already scheduled in the cluster for that specific AppGroup
func GetScheduledList(pods []*v1.Pod) ScheduledList {
	// scheduledList: Deployment name, replicaID, hostname
	scheduledList := ScheduledList{}

	for _, p := range pods {
		if len(p.Spec.NodeName) != 0 {
			scheduledInfo := ScheduledInfo{
				Name:      p.Name,
				Selector:  GetPodAppGroupSelector(p),
				ReplicaID: string(p.GetUID()),
				Hostname:  p.Spec.NodeName,
			}
			scheduledList = append(scheduledList, scheduledInfo)
		}
	}
	// Return the scheduledList
	return scheduledList
}
