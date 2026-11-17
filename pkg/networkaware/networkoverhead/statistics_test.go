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

package networkoverhead

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"

	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"
)

// TestFilterStatistics covers the bandwidth and cost checks of Filter against the intra
// statistics of the node's zone and segment.
func TestFilterStatistics(t *testing.T) {
	node := st.MakeNode().Name("n-1").
		Label(v1.LabelTopologyRegion, "us-west-1").
		Label(v1.LabelTopologyZone, "Z1").
		Label(string(ntv1alpha1.NetworkTopologySegment), "S1").Obj()

	intra := func(minBandwidth string, avgCost int64) ntv1alpha1.NetworkTopologyStatistics {
		return ntv1alpha1.NetworkTopologyStatistics{MinBandwidth: resource.MustParse(minBandwidth), AvgCost: avgCost}
	}

	tests := []struct {
		name         string
		origin       string // zone or segment the statistics belong to; "" for none
		stats        ntv1alpha1.NetworkTopologyStatistics
		agBandwidth  string
		agAvgCost    int64
		workload     agv1alpha1.AppGroupStatistics
		expectedCode fwk.Code
	}{
		{
			name:         "no statistics for the node's zone or segment: pass",
			origin:       "",
			agBandwidth:  "100Mi",
			agAvgCost:    5,
			expectedCode: fwk.Success,
		},
		{
			name:         "AppGroup without statistics (zero cost and bandwidth): pass",
			origin:       "Z1",
			stats:        intra("1Gi", 5),
			agBandwidth:  "0",
			agAvgCost:    0,
			expectedCode: fwk.Success,
		},
		{
			name:         "AppGroup cost above the zone's average cost: pass",
			origin:       "Z1",
			stats:        intra("1Gi", 5),
			agBandwidth:  "100Mi",
			agAvgCost:    10,
			expectedCode: fwk.Success,
		},
		{
			name:         "AppGroup cost at or below the zone's average cost: reject",
			origin:       "Z1",
			stats:        intra("1Gi", 5),
			agBandwidth:  "100Mi",
			agAvgCost:    5,
			expectedCode: fwk.Unschedulable,
		},
		{
			name:         "AppGroup bandwidth above the zone's minimum bandwidth: reject",
			origin:       "Z1",
			stats:        intra("100Mi", 5),
			agBandwidth:  "1Gi",
			agAvgCost:    10,
			expectedCode: fwk.Unschedulable,
		},
		{
			name:         "workload cost at or below the segment's average cost: reject",
			origin:       "S1",
			stats:        intra("1Gi", 5),
			agBandwidth:  "100Mi",
			agAvgCost:    10,
			workload:     agv1alpha1.AppGroupStatistics{AvgBandwidth: resource.MustParse("10Mi"), AvgCost: 3},
			expectedCode: fwk.Unschedulable,
		},
		{
			name:         "AppGroup cost at or below the segment's average cost: reject",
			origin:       "S1",
			stats:        intra("1Gi", 5),
			agBandwidth:  "100Mi",
			agAvgCost:    4,
			expectedCode: fwk.Unschedulable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statisticsMap := map[networkawareutil.StatisticsKey]ntv1alpha1.NetworkTopologyStatistics{}
			if tt.origin != "" {
				statisticsMap[networkawareutil.StatisticsKey{Origin: tt.origin, TypeStatistics: Intra}] = tt.stats
			}
			state := framework.NewCycleState()
			state.Write(preFilterStateKey, &PreFilterState{
				scoreEqually:                   true,
				appGroupStatisticsAvgBandwidth: resource.MustParse(tt.agBandwidth),
				appGroupStatisticsAvgCost:      tt.agAvgCost,
				workloadStatistics:             tt.workload,
				statisticsMap:                  statisticsMap,
			})

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(node)
			pl := &NetworkOverhead{}
			pod := makePod("p1", "p1-deployment", 0, "basic", nil, nil)

			got := pl.Filter(context.Background(), state, pod, nodeInfo)
			if got.Code() != tt.expectedCode {
				t.Errorf("expected %v, got %v: %v", tt.expectedCode, got.Code(), got.Message())
			}
		})
	}
}
