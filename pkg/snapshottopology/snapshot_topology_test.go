/*
Copyright 2026 The Kubernetes Authors.

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

package snapshottopology

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

const (
	regionKey = "topology.kubernetes.io/region"
	zoneKey   = "topology.kubernetes.io/zone"
)

// TestNodeMatchesTopology covers the Filter extension point's core decision:
// does a candidate node's labels satisfy the snapshot's NodeAffinity terms?
func TestNodeMatchesTopology(t *testing.T) {
	testcases := map[string]struct {
		nodeLabels map[string]string
		terms      []v1.TopologySelectorTerm
		want       bool
	}{
		// With zero terms there is nothing to OR over, so no node "matches".
		// The backward-compatible "empty topology => schedulable anywhere"
		// guarantee is enforced one level up in Filter, which short-circuits
		// to Success before calling nodeMatchesTopology when terms is empty.
		"no terms matches nothing (Filter short-circuits before this)": {
			nodeLabels: map[string]string{zoneKey: "us-west-2a"},
			terms:      nil,
			want:       false,
		},
		"node zone in the term values matches": {
			nodeLabels: map[string]string{zoneKey: "us-west-2a"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a", "us-west-2b"}},
				},
			}},
			want: true,
		},
		"node zone not in the term values does not match": {
			nodeLabels: map[string]string{zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a", "us-west-2b"}},
				},
			}},
			want: false,
		},
		"missing label key on node does not match": {
			nodeLabels: map[string]string{regionKey: "us-west-2"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: false,
		},
		"multiple expressions in a term are ANDed - all satisfied": {
			nodeLabels: map[string]string{regionKey: "us-west-2", zoneKey: "us-west-2a"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: regionKey, Values: []string{"us-west-2"}},
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: true,
		},
		"multiple expressions in a term are ANDed - one unsatisfied": {
			nodeLabels: map[string]string{regionKey: "us-west-2", zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: regionKey, Values: []string{"us-west-2"}},
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: false,
		},
		"multiple terms are ORed - second term matches": {
			nodeLabels: map[string]string{zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				}},
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2c"}},
				}},
			},
			want: true,
		},
		"multiple terms are ORed - none match": {
			nodeLabels: map[string]string{zoneKey: "us-west-2d"},
			terms: []v1.TopologySelectorTerm{
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				}},
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2c"}},
				}},
			},
			want: false,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			if got := nodeMatchesTopology(tc.nodeLabels, tc.terms); got != tc.want {
				t.Errorf("nodeMatchesTopology() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetPVCName verifies extraction of the PVC name from a pod volume,
// which gates whether PreFilter inspects a volume at all.
func TestGetPVCName(t *testing.T) {
	testcases := map[string]struct {
		vol  v1.Volume
		want string
	}{
		"pvc volume returns claim name": {
			vol: v1.Volume{
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						ClaimName: "ebs-claim",
					},
				},
			},
			want: "ebs-claim",
		},
		"non-pvc volume returns empty": {
			vol: v1.Volume{
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
			},
			want: "",
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			if got := getPVCName(tc.vol); got != tc.want {
				t.Errorf("getPVCName() = %q, want %q", got, tc.want)
			}
		})
	}
}
