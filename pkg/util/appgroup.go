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
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sort"
	"strings"
)

// Sort AppGroupTopologyList by Workload.Selector
type ByWorkloadSelector v1alpha1.AppGroupTopologyList

func (s ByWorkloadSelector) Len() int {
	return len(s)
}

func (s ByWorkloadSelector) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s ByWorkloadSelector) Less(i, j int) bool {
	return s[i].Workload.Selector < s[j].Workload.Selector
}

// Sort AppGroupWorkloadList by Workload.Selector
type BySelector v1alpha1.AppGroupWorkloadList

func (s BySelector) Len() int {
	return len(s)
}

func (s BySelector) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s BySelector) Less(i, j int) bool {
	return s[i].Workload.Selector < s[j].Workload.Selector
}

// FindWorkloadBySelector : return the workload's Info based on its selector
func FindWorkloadBySelector(workloadList v1alpha1.AppGroupWorkloadList, selector string) *v1alpha1.AppGroupWorkloadInfo {
	low := 0
	high := len(workloadList) - 1

	for low <= high {
		mid := (low + high) / 2
		if workloadList[mid].Workload.Selector == selector {
			return &workloadList[mid].Workload // Return the WorkloadInfo
		} else if workloadList[mid].Workload.Selector < selector {
			low = mid + 1
		} else if workloadList[mid].Workload.Selector > selector {
			high = mid - 1
		}
	}
	// Workload Info was not found
	return nil
}

// GetPodAppGroupLabel : get AppGroup from pod annotations
func GetPodAppGroupLabel(pod *v1.Pod) string {
	return pod.Labels[v1alpha1.AppGroupLabel]
}

// GetPodAppGroupSelector : get Workload Selector from pod annotations
func GetPodAppGroupSelector(pod *v1.Pod) string {
	return pod.Labels[v1alpha1.AppGroupSelectorLabel]
}

// Implementation of Topology Sorting algorithms based on https://github.com/otaviokr/topological-sort
// KahnSort : receives a tree (AppGroup Service Topology) and returns an array with the pods sorted.
func KahnSort(tree map[string][]string) ([]string, error) {
	var sorted []string
	inDegree := map[string]int{}

	// Normalize the tree to ensure all nodes are referred in the map.
	normalizedTree := NormalizeTree(tree)

	// 1 - Calculate the inDegree of all vertices by going through every edge of the graph
	// Each child gets inDegree++ during breadth-first run.
	for element, children := range normalizedTree {
		if _, exists := inDegree[element]; !exists {
			inDegree[element] = 0 // So far, element does not have any parent.
		}

		for _, child := range children {
			if _, exists := inDegree[child]; !exists {
				inDegree[child] = 1 // Being a child of an element, it is already a inDegree 1.
			} else {
				inDegree[child]++
			}
		}
	}

	// 2 - Collect all vertices with inDegree = 0 onto a stack
	stack := []string{}
	for element, value := range inDegree {
		if value == 0 {
			stack = append(stack, element)
			inDegree[element] = -1
		}
	}

	// 3 - While zero-degree-stack is not empty
	for len(stack) > 0 {
		// Pop element from zero-degree-stack and append it to topological order
		var node string
		node = stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		// Find all children of element and decrease inDegree. If inDegree becomes 0, add to zero-degree-stack
		for _, child := range normalizedTree[node] {
			inDegree[child]--
			if inDegree[child] == 0 {
				stack = append(stack, child)
				inDegree[child] = -1
			}
		}

		// Append to the sorted list.
		sorted = append(sorted, node)
	}

	if len(normalizedTree) != len(sorted) {
		// It seems that there's a directed cycle. Topological Sorting does not work for DAGs!
		var cycle []string
		for element, value := range inDegree {
			if value > 0 {
				cycle = append(cycle, element)
			}
		}

		sort.Slice(cycle, func(i, j int) bool {
			return cycle[i] < cycle[j]
		})

		return []string{}, fmt.Errorf("cycle involving elements: %s ", strings.Join(cycle, ", "))
	}

	return sorted, nil
}

// TarjanSort : receives a description of a search tree and returns a sorted array based on the Tarjan Sort algorithm.
func TarjanSort(tree map[string][]string) ([]string, error) {

	// Normalize the tree to ensure all nodes are referred in the map.
	normalizedTree := NormalizeTree(tree)

	var visitFunc func(string) error
	auxSorted := make([]string, len(normalizedTree))
	index := len(normalizedTree)
	temporary := map[string]bool{}
	visited := map[string]bool{}

	visitFunc = func(node string) error {
		switch {
		case temporary[node]:
			// Cycle found!
			return fmt.Errorf("found cycle at node: %s", node)
		case visited[node]:
			// Already visited. Moving on...
			return nil
		}

		temporary[node] = true // Mark as temporary to check for cycles...
		for _, child := range normalizedTree[node] {
			err := visitFunc(child) // Visit all children of a node
			if err != nil {
				return err
			}
		}

		delete(temporary, node)
		visited[node] = true
		index--
		auxSorted[index] = node
		return nil
	}

	for element := range normalizedTree {
		if visited[element] {
			continue
		}

		err := visitFunc(element)
		if err != nil {
			return []string{}, err
		}
	}

	var sorted []string
	for _, node := range auxSorted {
		if len(node) > 0 {
			sorted = append(sorted, node)
		}
	}

	return sorted, nil
}

// Reverse : inverts the order given by a Topology Sorting algorithm (e.g., Kahn, Tarjan).
func Reverse(tree map[string][]string, algorithm string) ([]string, error) {
	var reversed []string
	var sorted []string
	var err error

	switch algorithm {
	case v1alpha1.AppGroupKahnSort:
		sorted, err = KahnSort(tree)
		break
	case v1alpha1.AppGroupTarjanSort:
		sorted, err = TarjanSort(tree)
		break
	}

	if err != nil {
		return []string{}, err
	}

	for i := len(sorted); i > 0; i-- {
		reversed = append(reversed, sorted[i-1])
	}

	return reversed, nil
}

// Alternate : inverts the order given by a Topology Sorting algorithm (e.g., Kahn, Tarjan).
// Example: [1, N-1, 2, N-2, ...] (element indices)
func Alternate(tree map[string][]string, algorithm string) ([]string, error) {
	var alternate []string
	var sorted []string
	var err error

	switch algorithm {
	case v1alpha1.AppGroupKahnSort:
		sorted, err = KahnSort(tree)
		break
	case v1alpha1.AppGroupTarjanSort:
		sorted, err = TarjanSort(tree)
		break
	}

	if err != nil {
		return []string{}, err
	}

	klog.V(6).Info("Sorted: ", sorted)

	for i, j := 0, 0; i < len(sorted); i++ {
		if i%2 == 0 {
			alternate = append(alternate, sorted[j])
			j = j + 1
		} else {
			alternate = append(alternate, sorted[len(sorted)-j])
		}
	}

	klog.V(6).Info("Alternate: ", alternate)
	return alternate, nil
}

// ReverseKahn : reverses the order of the elements given by KahnSort in the resulting sorted list.
func ReverseKahn(tree map[string][]string) ([]string, error) {
	return Reverse(tree, v1alpha1.AppGroupKahnSort)
}

// ReverseTarjan : reverses the order of the elements given by TarjanSort in the resulting sorted list.
func ReverseTarjan(tree map[string][]string) ([]string, error) {
	return Reverse(tree, v1alpha1.AppGroupTarjanSort)
}

// AlternateKahn : inverts the order of the elements given by KahnSort in the resulting sorted list.
func AlternateKahn(tree map[string][]string) ([]string, error) {
	return Alternate(tree, v1alpha1.AppGroupKahnSort)
}

// AlternateTarjan : inverts the order of the elements given by TarjanSort in the resulting sorted list.
func AlternateTarjan(tree map[string][]string) ([]string, error) {
	return Alternate(tree, v1alpha1.AppGroupTarjanSort)
}

// NormalizeTree: checks if all Pods referred in the slices are present in the map as key too.
// If not, it will create the entry to make sure all nodes are accounted for.
func NormalizeTree(source map[string][]string) map[string][]string {
	normalized := map[string][]string{}

	for key, values := range source {
		// Copy the valid entry from the source map to the normalized map.
		normalized[key] = values
		for _, node := range values {
			if _, found := source[node]; !found {
				// Current node is in the slice, but not as a key in map.
				// This means we need to treat it as a leaf-node.
				normalized[node] = []string{}
			}
		}
	}
	return normalized
}
