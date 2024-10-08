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

package topologicalsort

import (
	"context"
	"math"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"

	"sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"
)

func GetAppGroupCROnlineBoutique() *agv1alpha1.AppGroup {
	// Return AppGroup CRD: onlineboutique
	return &agv1alpha1.AppGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "onlineboutique", Namespace: "default"},
		Spec: agv1alpha1.AppGroupSpec{NumMembers: 11, TopologySortingAlgorithm: "KahnSort",
			Workloads: agv1alpha1.AppGroupWorkloadList{
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1-deployment", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, // frontend
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4-deployment", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6-deployment", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8-deployment", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9-deployment", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10-deployment", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, // cartService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11-deployment", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, // productCatalogService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4-deployment", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}, // currencyService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5-deployment", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}, // paymentService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6-deployment", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}, // shippingService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7-deployment", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}, // emailService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8-deployment", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}, // checkoutService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4-deployment", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5-deployment", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6-deployment", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7-deployment", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9-deployment", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}, // recommendationService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
					}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10-deployment", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}, // adService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11-deployment", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}, // redis-cart
				},
			},
		},
		Status: agv1alpha1.AppGroupStatus{
			ScheduleStartTime:       metav1.Now(),
			TopologyCalculationTime: metav1.Now(),
			TopologyOrder: agv1alpha1.AppGroupTopologyList{
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1-deployment", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10-deployment", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9-deployment", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8-deployment", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}, Index: 4},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7-deployment", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}, Index: 5},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6-deployment", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}, Index: 6},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5-deployment", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}, Index: 7},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4-deployment", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}, Index: 8},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, Index: 9},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, Index: 10},
				agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11-deployment", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}, Index: 11},
			},
		},
	}
}

func GetAppGroupCRBasic() *agv1alpha1.AppGroup {
	// Return AppGroup CRD: basic
	return &agv1alpha1.AppGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "basic", Namespace: "default"},
		Spec: agv1alpha1.AppGroupSpec{
			NumMembers:               3,
			TopologySortingAlgorithm: "KahnSort",
			Workloads: agv1alpha1.AppGroupWorkloadList{
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1-deployment", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"},
					Dependencies: agv1alpha1.DependenciesList{agv1alpha1.DependenciesInfo{
						Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}}}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"},
					Dependencies: agv1alpha1.DependenciesList{agv1alpha1.DependenciesInfo{
						Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}}}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
		Status: agv1alpha1.AppGroupStatus{
			RunningWorkloads:        3,
			ScheduleStartTime:       metav1.Now(),
			TopologyCalculationTime: metav1.Now(),
			TopologyOrder: agv1alpha1.AppGroupTopologyList{
				agv1alpha1.AppGroupTopologyInfo{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1-deployment", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
				agv1alpha1.AppGroupTopologyInfo{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-deployment", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
				agv1alpha1.AppGroupTopologyInfo{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
			},
		},
	}
}

func TestTopologicalSortLess(t *testing.T) {
	// Get AppGroup CRD: basic
	basicAppGroup := GetAppGroupCRBasic()

	// Get AppGroup CRD: onlineboutique
	onlineBoutiqueAppGroup := GetAppGroupCROnlineBoutique()

	tests := []struct {
		name                     string
		namespace                string
		pInfo1                   *framework.QueuedPodInfo
		pInfo2                   *framework.QueuedPodInfo
		want                     bool
		agName                   string
		appGroup                 *agv1alpha1.AppGroup
		numMembers               int32
		selectors                []string
		deploymentNames          []string
		podPhase                 v1.PodPhase
		podNextPhase             v1.PodPhase
		topologySortingAlgorithm string
		desiredRunningWorkloads  int32
		desiredTopologyOrder     agv1alpha1.AppGroupTopologyList
		appGroupCreateTime       *metav1.Time
	}{
		{
			name:                     "basic, same AppGroup, p1 order lower than p2",
			agName:                   "basic",
			appGroup:                 basicAppGroup.DeepCopy(),
			namespace:                "default",
			numMembers:               3,
			selectors:                []string{"p1", "p2", "p3"},
			deploymentNames:          []string{"p1-deployment", "p2-deployment", "p3-deployment"},
			desiredRunningWorkloads:  3,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: "KahnSort",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p1", "p1-deployment", 0, "basic", nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p2", "p2-deployment", 0, "basic", nil, nil)),
			},
			desiredTopologyOrder: basicAppGroup.Status.TopologyOrder,
			want:                 true,
		},
		{
			name:                     "onlineboutique, same AppGroup, p5 order higher than p1",
			agName:                   "onlineboutique",
			appGroup:                 onlineBoutiqueAppGroup.DeepCopy(),
			namespace:                "default",
			numMembers:               11,
			selectors:                []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames:          []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment", "p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  11,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: "KahnSort",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p5", "p5-deployment", 0, "onlineboutique", nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil)),
			},
			desiredTopologyOrder: onlineBoutiqueAppGroup.Status.TopologyOrder,
			want:                 false,
		},
		{
			name:                     "pods from different AppGroups... ",
			agName:                   "basic",
			appGroup:                 basicAppGroup.DeepCopy(),
			namespace:                "default",
			numMembers:               3,
			selectors:                []string{"p1", "p2", "p3"},
			deploymentNames:          []string{"p1-deployment", "p2-deployment", "p3-deployment"},
			desiredRunningWorkloads:  3,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: "TarjanSort",
			pInfo1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p1", "p1-deployment", 0, "basic", nil, nil)),
			},
			pInfo2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, makePod("p5", "p5-deployment", 0, "other", nil, nil)),
			},
			desiredTopologyOrder: basicAppGroup.Status.TopologyOrder,
			want:                 false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := makePodsAppGroup(tt.deploymentNames, tt.agName, tt.podPhase)

			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			client := fake.NewClientBuilder().
				WithScheme(s).
				WithRuntimeObjects(pods...).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				Build()

			// Sort TopologyList by Selector
			sort.Sort(util.ByWorkloadSelector(tt.appGroup.Status.TopologyOrder))
			if err := client.Create(context.TODO(), tt.appGroup); err != nil {
				t.Errorf("failed to create AppGroup CR: %v", err)
			}

			ts := &TopologicalSort{
				Client:     client,
				namespaces: []string{metav1.NamespaceDefault},
			}

			if got := ts.Less(tt.pInfo1, tt.pInfo2); got != tt.want {
				t.Errorf("Less() = %v, want %v", got, tt.want)
			}
		})
	}
}

func BenchmarkTopologicalSortPlugin(b *testing.B) {
	ctx := context.TODO()
	agName := "onlineboutique"
	var desiredRunningWorkloads int32 = 11
	var numMembers int32 = 11
	namespace := "default"
	topologySortingAlgorithm := "KahnSort"

	// Get AppGroup CRD: onlineboutique
	onlineBoutiqueAppGroup := GetAppGroupCROnlineBoutique()

	tests := []struct {
		name                     string
		namespace                string
		want                     bool
		agName                   string
		appGroup                 *agv1alpha1.AppGroup
		numMembers               int32
		selectors                []string
		deploymentNames          []string
		podNum                   int
		podPhase                 v1.PodPhase
		podNextPhase             v1.PodPhase
		topologySortingAlgorithm string
		desiredTopologyOrder     agv1alpha1.AppGroupTopologyList
		desiredRunningWorkloads  int32
		appGroupCreateTime       *metav1.Time
	}{
		{
			name:       "onlineboutique AppGroup, 10 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     10,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 100 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     100,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 500 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     500,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 1000 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     1000,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 2000 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     2000,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 3000 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     3000,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 5000 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     5000,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
		{
			name:       "onlineboutique AppGroup, 10000 Pods",
			agName:     agName,
			appGroup:   onlineBoutiqueAppGroup,
			namespace:  namespace,
			numMembers: numMembers,
			podNum:     10000,
			selectors:  []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"},
			deploymentNames: []string{"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment", "p5-deployment",
				"p6-deployment", "p7-deployment", "p8-deployment", "p9-deployment", "p10-deployment", "p11-deployment"},
			desiredRunningWorkloads:  desiredRunningWorkloads,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: topologySortingAlgorithm,
			desiredTopologyOrder:     onlineBoutiqueAppGroup.Status.TopologyOrder,
		},
	}
	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {

			pods := makePodsAppGroup(tt.deploymentNames, tt.agName, tt.podPhase)

			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			client := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.appGroup).
				WithRuntimeObjects(pods...).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				Build()

			ts := &TopologicalSort{
				Client:     client,
				namespaces: []string{metav1.NamespaceDefault},
			}

			pInfo1 := getPodInfos(b, tt.podNum, tt.agName, tt.selectors, tt.deploymentNames)
			pInfo2 := getPodInfos(b, tt.podNum, tt.agName, tt.selectors, tt.deploymentNames)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sorting := func(i int) {
					_ = ts.Less(pInfo1[i], pInfo2[i])
				}
				Until(ctx, len(pInfo1), sorting)
			}
		})
	}
}

func randomInt(min int, max int) int {
	return min + rand.Intn(max-min)
}

func getPodInfos(b *testing.B, podsNum int, agName string, selectors []string, podNames []string) (pInfo []*framework.QueuedPodInfo) {
	pInfo = []*framework.QueuedPodInfo{}

	for i := 0; i < podsNum; i++ {
		random := randomInt(0, len(podNames))
		pInfo = append(pInfo, &framework.QueuedPodInfo{PodInfo: testutil.MustNewPodInfo(b, makePod(selectors[random], podNames[random], 0, agName, nil, nil))})
	}
	return pInfo
}

const parallelism = 16

// Copied from k8s internal package
// chunkSizeFor returns a chunk size for the given number of items to use for
// parallel work. The size aims to produce good CPU utilization.
func chunkSizeFor(n int) workqueue.Options {
	s := int(math.Sqrt(float64(n)))
	if r := n/parallelism + 1; s > r {
		s = r
	} else if s < 1 {
		s = 1
	}
	return workqueue.WithChunkSize(s)
}

// Copied from k8s internal package
// Until is a wrapper around workqueue.ParallelizeUntil to use in scheduling algorithms.
func Until(ctx context.Context, pieces int, doWorkPiece workqueue.DoWorkPieceFunc) {
	workqueue.ParallelizeUntil(ctx, parallelism, pieces, doWorkPiece, chunkSizeFor(pieces))
}

func makePodsAppGroup(podNames []string, agName string, phase v1.PodPhase) []runtime.Object {
	pds := make([]runtime.Object, 0)
	for _, name := range podNames {
		pod := st.MakePod().Namespace("default").Name(name).Obj()
		pod.Labels = map[string]string{agv1alpha1.AppGroupLabel: agName}
		pod.Status.Phase = phase
		pds = append(pds, pod)
	}
	return pds
}

func makePod(selector string, podName string, priority int32, appGroup string, requests, limits v1.ResourceList) *v1.Pod {
	label := make(map[string]string)
	label[agv1alpha1.AppGroupLabel] = appGroup
	label[agv1alpha1.AppGroupSelectorLabel] = selector

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: label,
		},
		Spec: v1.PodSpec{
			Priority: &priority,
			Containers: []v1.Container{
				{
					Name: podName,
					Resources: v1.ResourceRequirements{
						Requests: requests,
						Limits:   limits,
					},
				},
			},
		},
	}
}
