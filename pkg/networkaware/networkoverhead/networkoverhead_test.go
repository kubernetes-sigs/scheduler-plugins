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

package networkoverhead

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	schedruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
	"github.com/stretchr/testify/assert"
)

var _ framework.SharedLister = &testSharedLister{}

type testSharedLister struct {
	nodes       []*v1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func (f *testSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *testSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *testSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *testSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

func GetNetworkTopologyCR() *ntv1alpha1.NetworkTopology {
	// Return NetworkTopology CR: nt-test
	return &ntv1alpha1.NetworkTopology{
		ObjectMeta: metav1.ObjectMeta{Name: "nt-test", Namespace: "default"},
		Spec: ntv1alpha1.NetworkTopologySpec{
			Weights: ntv1alpha1.WeightList{
				ntv1alpha1.WeightInfo{Name: "UserDefined",
					TopologyList: ntv1alpha1.TopologyList{
						ntv1alpha1.TopologyInfo{
							TopologyKey: "topology.kubernetes.io/region",
							OriginList: ntv1alpha1.OriginList{
								ntv1alpha1.OriginInfo{Origin: "R1", CostList: []ntv1alpha1.CostInfo{
									{Destination: "R2", NetworkCost: 50},
									{Destination: "R3", NetworkCost: 50},
									{Destination: "R4", NetworkCost: 50},
									{Destination: "R5", NetworkCost: 50}},
								},
								ntv1alpha1.OriginInfo{Origin: "R2", CostList: []ntv1alpha1.CostInfo{
									{Destination: "R1", NetworkCost: 50},
									{Destination: "R3", NetworkCost: 50},
									{Destination: "R4", NetworkCost: 50},
									{Destination: "R5", NetworkCost: 50}},
								},
								ntv1alpha1.OriginInfo{Origin: "R3", CostList: []ntv1alpha1.CostInfo{
									{Destination: "R1", NetworkCost: 50},
									{Destination: "R2", NetworkCost: 50},
									{Destination: "R4", NetworkCost: 50},
									{Destination: "R5", NetworkCost: 50}},
								},
								ntv1alpha1.OriginInfo{Origin: "R4", CostList: []ntv1alpha1.CostInfo{
									{Destination: "R1", NetworkCost: 50},
									{Destination: "R2", NetworkCost: 50},
									{Destination: "R3", NetworkCost: 50},
									{Destination: "R5", NetworkCost: 50}},
								},
								ntv1alpha1.OriginInfo{Origin: "R5", CostList: []ntv1alpha1.CostInfo{
									{Destination: "R1", NetworkCost: 50},
									{Destination: "R2", NetworkCost: 50},
									{Destination: "R3", NetworkCost: 50},
									{Destination: "R4", NetworkCost: 50}},
								},
							}},
						ntv1alpha1.TopologyInfo{
							TopologyKey: "topology.kubernetes.io/region",
							OriginList: ntv1alpha1.OriginList{
								ntv1alpha1.OriginInfo{Origin: "Z1", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10}, {Destination: "Z4", NetworkCost: 10},
									{Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z2", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10}, {Destination: "Z4", NetworkCost: 10},
									{Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z3", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z4", NetworkCost: 10},
									{Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z4", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z5", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z6", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z5", NetworkCost: 10}, {Destination: "Z7", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z7", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10},
									{Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z8", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10},
									{Destination: "Z7", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z9", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10},
									{Destination: "Z7", NetworkCost: 10}, {Destination: "Z8", NetworkCost: 10}, {Destination: "Z10", NetworkCost: 10}},
								},
								ntv1alpha1.OriginInfo{Origin: "Z10", CostList: []ntv1alpha1.CostInfo{
									{Destination: "Z1", NetworkCost: 10}, {Destination: "Z2", NetworkCost: 10}, {Destination: "Z3", NetworkCost: 10},
									{Destination: "Z4", NetworkCost: 10}, {Destination: "Z5", NetworkCost: 10}, {Destination: "Z6", NetworkCost: 10},
									{Destination: "Z7", NetworkCost: 10}, {Destination: "Z8", NetworkCost: 10}, {Destination: "Z9", NetworkCost: 10}},
								},
							},
						},
					},
				},
			},
		},
	}
}

func GetNetworkTopologyCRBasic() *ntv1alpha1.NetworkTopology {
	// Return NetworkTopology CR (basic version): nt-test
	return &ntv1alpha1.NetworkTopology{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nt-test",
			Namespace: "default",
			UID:       types.UID("fake-uid"),
		},
		Spec: ntv1alpha1.NetworkTopologySpec{
			Weights: ntv1alpha1.WeightList{
				ntv1alpha1.WeightInfo{Name: "UserDefined",
					TopologyList: ntv1alpha1.TopologyList{
						ntv1alpha1.TopologyInfo{
							TopologyKey: "topology.kubernetes.io/region",
							OriginList: ntv1alpha1.OriginList{
								ntv1alpha1.OriginInfo{
									Origin:   "us-west-1",
									CostList: []ntv1alpha1.CostInfo{{Destination: "us-east-1", NetworkCost: 20}}},
								ntv1alpha1.OriginInfo{
									Origin:   "us-east-1",
									CostList: []ntv1alpha1.CostInfo{{Destination: "us-west-1", NetworkCost: 20}}},
							}},
						ntv1alpha1.TopologyInfo{
							TopologyKey: "topology.kubernetes.io/zone",
							OriginList: ntv1alpha1.OriginList{
								ntv1alpha1.OriginInfo{Origin: "Z1", CostList: []ntv1alpha1.CostInfo{{Destination: "Z2", NetworkCost: 5}}},
								ntv1alpha1.OriginInfo{Origin: "Z2", CostList: []ntv1alpha1.CostInfo{{Destination: "Z1", NetworkCost: 5}}},
								ntv1alpha1.OriginInfo{Origin: "Z3", CostList: []ntv1alpha1.CostInfo{{Destination: "Z4", NetworkCost: 10}}},
								ntv1alpha1.OriginInfo{Origin: "Z4", CostList: []ntv1alpha1.CostInfo{{Destination: "Z3", NetworkCost: 10}}},
							},
						},
					},
				},
			},
		},
	}
}

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
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-deployment", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "basic",
			Namespace: "default",
			UID:       types.UID("fake-uid"),
		},
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

func BenchmarkNetworkOverheadPreFilter(b *testing.B) {
	// Get AppGroup CRD: onlineboutique
	onlineBoutiqueAppGroup := GetAppGroupCROnlineBoutique()

	// Get Network Topology CRD: nt-test
	networkTopology := GetNetworkTopologyCR()

	regionNames := []string{"R1", "R2", "R3", "R4", "R5"}
	zoneNames := []string{"Z1", "Z2", "Z3", "Z4", "Z5", "Z6", "Z7", "Z8", "Z9", "Z10"}

	pods := []*v1.Pod{
		makePodAllocated("p1", "p1-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p2", "p2-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p3", "p3-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p4", "p4-deployment", "n-9", 0, "onlineboutique", nil, nil),
		makePodAllocated("p5", "p5-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p6", "p6-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p7", "p7-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p8", "p8-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p9", "p9-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p10", "p10-deployment", "n-2", 0, "onlineboutique", nil, nil),
	}

	tests := []struct {
		name            string
		nodesNum        int64
		dependenciesNum int32
		agName          string
		WorkloadNames   []string
		regionNames     []string
		zoneNames       []string
		appGroup        *agv1alpha1.AppGroup
		networkTopology *ntv1alpha1.NetworkTopology
		pod             *v1.Pod
		pods            []*v1.Pod
		expected        framework.Code
	}{
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10 nodes, 1 pod to allocate",
			nodesNum:        10,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 100 nodes, 1 pod to allocate",
			nodesNum:        100,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 500 nodes, 1 pod to allocate",
			nodesNum:        500,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 1000 nodes, 1 pod to allocate",
			nodesNum:        1000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 2000 nodes, 1 pod to allocate",
			nodesNum:        2000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 3000 nodes, 1 pod to allocate",
			nodesNum:        3000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 5000 nodes, 1 pod to allocate",
			nodesNum:        5000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10000 nodes, 1 pod to allocate",
			nodesNum:        10000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			utilruntime.Must(ntv1alpha1.AddToScheme(s))

			// init nodes
			nodes := getNodes(tt.nodesNum, tt.regionNames, tt.zoneNames)

			// Create dependencies
			tt.appGroup.Status.RunningWorkloads = tt.dependenciesNum

			cs := testClientSet.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			builder := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.appGroup, tt.networkTopology).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				WithStatusSubresource(&ntv1alpha1.NetworkTopology{})
			for _, p := range tt.pods {
				builder.WithObjects(p.DeepCopy())
			}
			client := builder.Build()

			// create plugin
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			snapshot := newTestSharedLister(nil, nodes)

			podInformer := informerFactory.Core().V1().Pods()
			podLister := podInformer.Lister()

			informerFactory.Start(ctx.Done())

			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
				if err != nil {
					b.Fatalf("Failed to create Workload %q: %v", p.Name, err)
				}
			}

			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, _ := tf.NewFramework(ctx, registeredPlugins, "default-scheduler", schedruntime.WithClientSet(cs),
				schedruntime.WithInformerFactory(informerFactory), schedruntime.WithSnapshotSharedLister(snapshot))

			pl := &NetworkOverhead{
				Client:      client,
				podLister:   podLister,
				handle:      fh,
				namespaces:  []string{"default"},
				weightsName: "UserDefined",
				ntName:      "nt-test",
			}

			state := framework.NewCycleState()

			// Wait for the pods to be scheduled.
			if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
				return true, nil
			}); err != nil {
				b.Errorf("pods not scheduled yet: %v ", err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Prefilter
				if _, got := pl.PreFilter(context.TODO(), state, tt.pod); got.Code() != tt.expected {
					b.Errorf("expected %v, got %v : %v", tt.expected, got.Code(), got.Message())
					assert.True(b, got.IsSuccess())
				}
			}
		})
	}
}

func TestNetworkOverheadScore(t *testing.T) {
	// Create AppGroup CRD: basic
	basicAppGroup := GetAppGroupCRBasic()

	// Create Network Topology CRD
	networkTopology := GetNetworkTopologyCRBasic()

	// Create Nodes
	nodes := []*v1.Node{
		st.MakeNode().Name("n-1").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z1").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-2").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z1").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-3").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z2").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-4").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z2").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-5").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z3").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-6").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z3").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-7").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z4").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-8").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z4").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
	}

	tests := []struct {
		name               string
		agName             string
		dependenciesNum    int32
		pods               []*v1.Pod
		appGroup           *agv1alpha1.AppGroup
		networkTopology    *ntv1alpha1.NetworkTopology
		nodes              []*v1.Node
		pod                *v1.Pod
		want               *framework.Status
		wantedScoresBefore framework.NodeScoreList
		wantedScoresAfter  framework.NodeScoreList
		nodeToScore        *v1.Node
		expected           framework.Code
	}{
		{
			name:            "AppGroup: basic, p1 to allocate, 8 nodes to score",
			agName:          "basic",
			appGroup:        basicAppGroup,
			dependenciesNum: 5,
			pods: []*v1.Pod{
				makePodAllocated("p1", "p1-deployment", "n-2", 0, "basic", nil, nil),
				makePodAllocated("p2", "p2-deployment", "n-5", 0, "basic", nil, nil),
				makePodAllocated("p3", "p3-deployment", "n-1", 0, "basic", nil, nil),
			},
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			want:            nil,
			wantedScoresBefore: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 20},
				framework.NodeScore{Name: nodes[1].Name, Score: 20},
				framework.NodeScore{Name: nodes[2].Name, Score: 20},
				framework.NodeScore{Name: nodes[3].Name, Score: 20},
				framework.NodeScore{Name: nodes[4].Name, Score: 0},
				framework.NodeScore{Name: nodes[5].Name, Score: 1},
				framework.NodeScore{Name: nodes[6].Name, Score: 10},
				framework.NodeScore{Name: nodes[7].Name, Score: 10},
			},
			wantedScoresAfter: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 0},
				framework.NodeScore{Name: nodes[1].Name, Score: 0},
				framework.NodeScore{Name: nodes[2].Name, Score: 0},
				framework.NodeScore{Name: nodes[3].Name, Score: 0},
				framework.NodeScore{Name: nodes[4].Name, Score: 100},
				framework.NodeScore{Name: nodes[5].Name, Score: 95},
				framework.NodeScore{Name: nodes[6].Name, Score: 50},
				framework.NodeScore{Name: nodes[7].Name, Score: 50},
			},
			expected: framework.Success,
		},
		{
			name:            "AppGroup: basic, p2 to allocate, 8 nodes to score",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p2", "p2-deployment", 0, "basic", nil, nil),
			pods: []*v1.Pod{
				makePodAllocated("p1", "p1-deployment", "n-2", 0, "basic", nil, nil),
				makePodAllocated("p2", "p2-deployment", "n-5", 0, "basic", nil, nil),
				makePodAllocated("p3", "p3-deployment", "n-1", 0, "basic", nil, nil),
			},
			nodes: nodes,
			want:  nil,
			wantedScoresBefore: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 0},
				framework.NodeScore{Name: nodes[1].Name, Score: 1},
				framework.NodeScore{Name: nodes[2].Name, Score: 5},
				framework.NodeScore{Name: nodes[3].Name, Score: 5},
				framework.NodeScore{Name: nodes[4].Name, Score: 20},
				framework.NodeScore{Name: nodes[5].Name, Score: 20},
				framework.NodeScore{Name: nodes[6].Name, Score: 20},
				framework.NodeScore{Name: nodes[7].Name, Score: 20},
			},
			wantedScoresAfter: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 100},
				framework.NodeScore{Name: nodes[1].Name, Score: 95},
				framework.NodeScore{Name: nodes[2].Name, Score: 75},
				framework.NodeScore{Name: nodes[3].Name, Score: 75},
				framework.NodeScore{Name: nodes[4].Name, Score: 0},
				framework.NodeScore{Name: nodes[5].Name, Score: 0},
				framework.NodeScore{Name: nodes[6].Name, Score: 0},
				framework.NodeScore{Name: nodes[7].Name, Score: 0},
			},
			expected: framework.Success,
		},
		{
			name:            "AppGroup: basic, p3 to allocate, no dependency, 8 nodes to score",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pods: []*v1.Pod{
				makePodAllocated("p1", "p1-deployment", "n-2", 0, "basic", nil, nil),
				makePodAllocated("p2", "p2-deployment", "n-5", 0, "basic", nil, nil),
				makePodAllocated("p3", "p3-deployment", "n-1", 0, "basic", nil, nil),
			},
			pod:   makePod("p3", "p3-deployment", 0, "basic", nil, nil),
			nodes: nodes,
			want:  nil,
			wantedScoresBefore: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 0},
				framework.NodeScore{Name: nodes[1].Name, Score: 0},
				framework.NodeScore{Name: nodes[2].Name, Score: 0},
				framework.NodeScore{Name: nodes[3].Name, Score: 0},
				framework.NodeScore{Name: nodes[4].Name, Score: 0},
				framework.NodeScore{Name: nodes[5].Name, Score: 0},
				framework.NodeScore{Name: nodes[6].Name, Score: 0},
				framework.NodeScore{Name: nodes[7].Name, Score: 0},
			},
			wantedScoresAfter: framework.NodeScoreList{
				framework.NodeScore{Name: nodes[0].Name, Score: 0},
				framework.NodeScore{Name: nodes[1].Name, Score: 0},
				framework.NodeScore{Name: nodes[2].Name, Score: 0},
				framework.NodeScore{Name: nodes[3].Name, Score: 0},
				framework.NodeScore{Name: nodes[4].Name, Score: 0},
				framework.NodeScore{Name: nodes[5].Name, Score: 0},
				framework.NodeScore{Name: nodes[6].Name, Score: 0},
				framework.NodeScore{Name: nodes[7].Name, Score: 0},
			},
			expected: framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			utilruntime.Must(ntv1alpha1.AddToScheme(s))

			ctx := context.Background()
			cs := testClientSet.NewSimpleClientset()
			builder := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tt.appGroup, tt.networkTopology).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				WithStatusSubresource(&ntv1alpha1.NetworkTopology{})
			for _, p := range tt.pods {
				builder.WithObjects(p.DeepCopy())
			}
			client := builder.Build()

			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := newTestSharedLister(nil, tt.nodes)

			podInformer := informerFactory.Core().V1().Pods()
			podLister := podInformer.Lister()

			informerFactory.Start(ctx.Done())

			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Workload %q: %v", p.Name, err)
				}
			}

			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, _ := tf.NewFramework(ctx, registeredPlugins, "default-scheduler", schedruntime.WithClientSet(cs),
				schedruntime.WithInformerFactory(informerFactory), schedruntime.WithSnapshotSharedLister(snapshot))

			pl := &NetworkOverhead{
				Client:      client,
				podLister:   podLister,
				handle:      fh,
				namespaces:  []string{"default"},
				weightsName: "UserDefined",
				ntName:      "nt-test",
			}

			// Wait for the pods to be scheduled.
			if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
				return true, nil
			}); err != nil {
				t.Errorf("pods not scheduled yet: %v ", err)
			}

			var scoreList framework.NodeScoreList
			state := framework.NewCycleState()

			for _, n := range nodes {
				// Prefilter
				if _, got := pl.PreFilter(ctx, state, tt.pod); got.Code() != tt.expected {
					t.Errorf("expected %v, got %v : %v", tt.expected, got.Code(), got.Message())
				}

				// Score
				score, gotStatus := pl.Score(
					ctx,
					state,
					tt.pod, n.Name)
				t.Logf("Workload: %v; Node: %v; score: %v; status: %v; message: %v \n", tt.pod.Name, n.Name, score, gotStatus.Code().String(), gotStatus.Message())

				nodeScore := framework.NodeScore{
					Name:  n.Name,
					Score: score,
				}
				scoreList = append(scoreList, nodeScore)
			}

			if !reflect.DeepEqual(tt.wantedScoresBefore, scoreList) {
				t.Errorf("[Score] status does not match: %v, want: %v\n", scoreList, tt.wantedScoresBefore)
			}

			pl.NormalizeScore(
				context.Background(),
				framework.NewCycleState(),
				tt.pod,
				scoreList)

			if !reflect.DeepEqual(tt.wantedScoresAfter, scoreList) {
				t.Errorf("[Normalize] status does not match: %v, want: %v\n", scoreList, tt.wantedScoresAfter)
			}
		})
	}
}

func BenchmarkNetworkOverheadScore(b *testing.B) {
	// Get AppGroup CRD: onlineboutique
	onlineBoutiqueAppGroup := GetAppGroupCROnlineBoutique()

	// Get Network Topology CRD: nt-test
	networkTopology := GetNetworkTopologyCR()

	regionNames := []string{"R1", "R2", "R3", "R4", "R5"}
	zoneNames := []string{"Z1", "Z2", "Z3", "Z4", "Z5", "Z6", "Z7", "Z8", "Z9", "Z10"}

	pods := []*v1.Pod{
		makePodAllocated("p1", "p1-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p2", "p2-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p3", "p3-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p4", "p4-deployment", "n-9", 0, "onlineboutique", nil, nil),
		makePodAllocated("p5", "p5-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p6", "p6-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p7", "p7-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p8", "p8-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p9", "p9-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p10", "p10-deployment", "n-2", 0, "onlineboutique", nil, nil),
	}

	tests := []struct {
		name            string
		nodesNum        int64
		dependenciesNum int32
		agName          string
		WorkloadNames   []string
		regionNames     []string
		zoneNames       []string
		appGroup        *agv1alpha1.AppGroup
		networkTopology *ntv1alpha1.NetworkTopology
		pod             *v1.Pod
		pods            []*v1.Pod
		expected        framework.Code
	}{
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10 nodes, 1 pod to allocate",
			nodesNum:        10,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 100 nodes, 1 pod to allocate",
			nodesNum:        100,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 500 nodes, 1 pod to allocate",
			nodesNum:        500,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 1000 nodes, 1 pod to allocate",
			nodesNum:        1000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 2000 nodes, 1 pod to allocate",
			nodesNum:        2000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 3000 nodes, 1 pod to allocate",
			nodesNum:        3000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 5000 nodes, 1 pod to allocate",
			nodesNum:        5000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10000 nodes, 1 pod to allocate",
			nodesNum:        10000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			utilruntime.Must(ntv1alpha1.AddToScheme(s))

			// init nodes
			nodes := getNodes(tt.nodesNum, tt.regionNames, tt.zoneNames)

			// Create dependencies
			tt.appGroup.Status.RunningWorkloads = tt.dependenciesNum

			// create plugin
			ctx := context.Background()
			cs := testClientSet.NewSimpleClientset()
			builder := fake.NewClientBuilder().
				WithScheme(s).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				WithStatusSubresource(&ntv1alpha1.NetworkTopology{}).
				WithObjects(tt.appGroup, tt.networkTopology)
			for _, p := range tt.pods {
				builder.WithObjects(p.DeepCopy())
			}
			client := builder.Build()

			informerFactory := informers.NewSharedInformerFactory(cs, 0)

			snapshot := newTestSharedLister(nil, nodes)
			podInformer := informerFactory.Core().V1().Pods()
			podLister := podInformer.Lister()

			informerFactory.Start(ctx.Done())

			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
				if err != nil {
					b.Fatalf("Failed to create Workload %q: %v", p.Name, err)
				}
			}

			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, _ := tf.NewFramework(ctx, registeredPlugins, "default-scheduler", schedruntime.WithClientSet(cs),
				schedruntime.WithInformerFactory(informerFactory), schedruntime.WithSnapshotSharedLister(snapshot))

			pl := &NetworkOverhead{
				Client:      client,
				podLister:   podLister,
				handle:      fh,
				namespaces:  []string{"default"},
				weightsName: "UserDefined",
				ntName:      "nt-test",
			}

			state := framework.NewCycleState()

			// Wait for the pods to be scheduled.
			if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
				return true, nil
			}); err != nil {
				b.Errorf("pods not scheduled yet: %v ", err)
			}

			// Prefilter
			if _, got := pl.PreFilter(context.TODO(), state, tt.pod); got.Code() != tt.expected {
				b.Errorf("expected %v, got %v : %v", tt.expected, got.Code(), got.Message())
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				gotList := make(framework.NodeScoreList, len(nodes))

				scoreNode := func(i int) {
					n := nodes[i]
					score, _ := pl.Score(ctx, state, tt.pod, n.Name)
					gotList[i] = framework.NodeScore{Name: n.Name, Score: score}
				}
				Until(ctx, len(nodes), scoreNode)

				status := pl.NormalizeScore(ctx, state, tt.pod, gotList)
				assert.True(b, status.IsSuccess())
			}
		})
	}
}

func TestNetworkOverheadFilter(t *testing.T) {
	// Get AppGroup CRD: basic
	basicAppGroup := GetAppGroupCRBasic()

	// Get Network Topology CR: nt-test
	networkTopology := GetNetworkTopologyCRBasic()

	// Create Pods
	pods := []*v1.Pod{
		makePodAllocated("p1", "p1-deployment", "n-2", 0, "basic", nil, nil),
		makePodAllocated("p2", "p2-deployment", "n-5", 0, "basic", nil, nil),
		makePodAllocated("p3", "p3-deployment", "n-8", 0, "basic", nil, nil),
	}

	// Create Nodes
	nodes := []*v1.Node{
		st.MakeNode().Name("n-1").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z1").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-2").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z1").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-3").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z2").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-4").Label(v1.LabelTopologyRegion, "us-west-1").Label(v1.LabelTopologyZone, "Z2").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-5").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z3").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-6").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z3").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-7").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z4").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
		st.MakeNode().Name("n-8").Label(v1.LabelTopologyRegion, "us-east-1").Label(v1.LabelTopologyZone, "Z4").Capacity(
			map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).Obj(),
	}

	tests := []struct {
		name            string
		agName          string
		appGroup        *agv1alpha1.AppGroup
		networkTopology *ntv1alpha1.NetworkTopology
		pod             *v1.Pod
		pods            []*v1.Pod
		nodes           []*v1.Node
		nodeToFilter    *v1.Node
		wantStatus      *framework.Status
		expected        framework.Code
	}{
		{
			name:            "AppGroup: basic, p1 to allocate, n-1 to filter: n-1 does not meet network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      framework.NewStatus(framework.Unschedulable, "Node n-1 does not meet several network requirements from Workload dependencies: Satisfied: 0 Violated: 1"),
			nodeToFilter:    nodes[0],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p1 to allocate, n-6 to filter: n-6 meets network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      nil,
			nodeToFilter:    nodes[5],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p2 to allocate, n-5 to filter: n-5 does not meet network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p2", "p2-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      framework.NewStatus(framework.Unschedulable, "Node n-5 does not meet several network requirements from Workload dependencies: Satisfied: 0 Violated: 1"),
			nodeToFilter:    nodes[4],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p2 to allocate, n-7 to filter: n-7 meets network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p2", "p2-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      nil,
			nodeToFilter:    nodes[6],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p3 to allocate, no dependencies, n-1 to filter: n-1 meets network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p3", "p3-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      nil,
			nodeToFilter:    nodes[0],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p10 to allocate, different AppGroup, n-1 to filter: n-1 meets network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p10", "p10-deployment", 0, "", nil, nil),
			nodes:           nodes,
			wantStatus:      nil,
			nodeToFilter:    nodes[0],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p1 to allocate, n-1 to filter, multiple dependencies: n-1 does not meet network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      framework.NewStatus(framework.Unschedulable, "Node n-1 does not meet several network requirements from Workload dependencies: Satisfied: 0 Violated: 1"),
			nodeToFilter:    nodes[0],
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: basic, p1 to allocate, n-6 to filter, multiple dependencies: n-6 meets network requirements",
			agName:          "basic",
			appGroup:        basicAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "basic", nil, nil),
			nodes:           nodes,
			wantStatus:      nil,
			nodeToFilter:    nodes[5],
			pods:            pods,
			expected:        framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			utilruntime.Must(ntv1alpha1.AddToScheme(s))

			// create plugin
			ctx := context.Background()
			cs := testClientSet.NewSimpleClientset()

			builder := fake.NewClientBuilder().
				WithScheme(s).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				WithStatusSubresource(&ntv1alpha1.NetworkTopology{}).
				WithObjects(tt.appGroup, tt.networkTopology)
			for _, p := range tt.pods {
				builder.WithObjects(p.DeepCopy())
			}
			client := builder.Build()

			informerFactory := informers.NewSharedInformerFactory(cs, 0)

			snapshot := newTestSharedLister(nil, nodes)

			podInformer := informerFactory.Core().V1().Pods()
			podLister := podInformer.Lister()

			informerFactory.Start(ctx.Done())

			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", p.Name, err)
				}
			}

			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, _ := tf.NewFramework(ctx, registeredPlugins, "default-scheduler",
				schedruntime.WithClientSet(cs),
				schedruntime.WithInformerFactory(informerFactory),
				schedruntime.WithSnapshotSharedLister(snapshot))

			pl := &NetworkOverhead{
				Client:      client,
				podLister:   podLister,
				handle:      fh,
				namespaces:  []string{"default"},
				weightsName: "UserDefined",
				ntName:      "nt-test",
			}

			// Wait for the pods to be scheduled.
			if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
				return true, nil
			}); err != nil {
				t.Errorf("pods not scheduled yet: %v ", err)
			}

			state := framework.NewCycleState()

			// Prefilter
			if _, got := pl.PreFilter(context.TODO(), state, tt.pod); got.Code() != tt.expected {
				t.Errorf("expected %v, got %v : %v", tt.expected, got.Code(), got.Message())
			}

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(tt.nodeToFilter)
			gotStatus := pl.Filter(context.Background(), state, tt.pod, nodeInfo)

			if !reflect.DeepEqual(gotStatus, tt.wantStatus) {
				t.Errorf("status does not match: %v, want: %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

func BenchmarkNetworkOverheadFilter(b *testing.B) {
	// Get AppGroup CRD: onlineboutique
	onlineBoutiqueAppGroup := GetAppGroupCROnlineBoutique()

	// Get Network Topology CRD: nt-test
	networkTopology := GetNetworkTopologyCR()

	regionNames := []string{"R1", "R2", "R3", "R4", "R5"}
	zoneNames := []string{"Z1", "Z2", "Z3", "Z4", "Z5", "Z6", "Z7", "Z8", "Z9", "Z10"}

	// Create Pods
	pods := []*v1.Pod{
		makePodAllocated("p1", "p1-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p2", "p2-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p3", "p3-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p4", "p4-deployment", "n-9", 0, "onlineboutique", nil, nil),
		makePodAllocated("p5", "p5-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p6", "p6-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p7", "p7-deployment", "n-2", 0, "onlineboutique", nil, nil),
		makePodAllocated("p8", "p8-deployment", "n-5", 0, "onlineboutique", nil, nil),
		makePodAllocated("p9", "p9-deployment", "n-1", 0, "onlineboutique", nil, nil),
		makePodAllocated("p10", "p10-deployment", "n-2", 0, "onlineboutique", nil, nil),
	}

	tests := []struct {
		name            string
		nodesNum        int64
		dependenciesNum int32
		agName          string
		podNames        []string
		regionNames     []string
		zoneNames       []string
		appGroup        *agv1alpha1.AppGroup
		networkTopology *ntv1alpha1.NetworkTopology
		pod             *v1.Pod
		pods            []*v1.Pod
		expected        framework.Code
	}{
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10 nodes, 1 pod to allocate",
			nodesNum:        10,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 100 nodes, 1 pod to allocate",
			nodesNum:        100,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 500 nodes, 1 pod to allocate",
			nodesNum:        500,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 1000 nodes, 1 pod to allocate",
			nodesNum:        1000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 2000 nodes, 1 pod to allocate",
			nodesNum:        2000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 3000 nodes, 1 pod to allocate",
			nodesNum:        3000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 5000 nodes, 1 pod to allocate",
			nodesNum:        5000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
		{
			name:            "AppGroup: onlineboutique, 10 pods allocated, 10000 nodes, 1 pod to allocate",
			nodesNum:        10000,
			dependenciesNum: 10,
			agName:          "onlineboutique",
			regionNames:     regionNames,
			zoneNames:       zoneNames,
			appGroup:        onlineBoutiqueAppGroup,
			networkTopology: networkTopology,
			pod:             makePod("p1", "p1-deployment", 0, "onlineboutique", nil, nil),
			pods:            pods,
			expected:        framework.Success,
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))
			utilruntime.Must(ntv1alpha1.AddToScheme(s))

			// init nodes
			nodes := getNodes(tt.nodesNum, tt.regionNames, tt.zoneNames)

			// create plugin
			ctx := context.Background()
			cs := testClientSet.NewSimpleClientset()
			builder := fake.NewClientBuilder().
				WithScheme(s).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				WithStatusSubresource(&ntv1alpha1.NetworkTopology{}).
				WithObjects(tt.appGroup, tt.networkTopology)
			for _, p := range tt.pods {
				builder.WithObjects(p.DeepCopy())
			}
			client := builder.Build()

			informerFactory := informers.NewSharedInformerFactory(cs, 0)

			snapshot := newTestSharedLister(nil, nodes)

			podInformer := informerFactory.Core().V1().Pods()
			podLister := podInformer.Lister()

			informerFactory.Start(ctx.Done())

			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
				if err != nil {
					b.Fatalf("Failed to create Workload %q: %v", p.Name, err)
				}
			}

			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
			}

			fh, _ := tf.NewFramework(ctx, registeredPlugins, "default-scheduler",
				schedruntime.WithClientSet(cs),
				schedruntime.WithInformerFactory(informerFactory),
				schedruntime.WithSnapshotSharedLister(snapshot))

			pl := &NetworkOverhead{
				Client:      client,
				podLister:   podLister,
				handle:      fh,
				namespaces:  []string{"default"},
				weightsName: "UserDefined",
				ntName:      "nt-test",
			}

			// Wait for the pods to be scheduled.
			if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
				return true, nil
			}); err != nil {
				b.Errorf("pods not scheduled yet: %v ", err)
			}

			state := framework.NewCycleState()

			// Prefilter
			if _, got := pl.PreFilter(context.TODO(), state, tt.pod); got.Code() != tt.expected {
				b.Errorf("expected %v, got %v : %v", tt.expected, got.Code(), got.Message())
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				filter := func(i int) {
					nodeInfo := framework.NewNodeInfo()
					nodeInfo.SetNode(nodes[i])
					_ = pl.Filter(ctx, state, tt.pod, nodeInfo)
				}
				Until(ctx, len(nodes), filter)
			}
		})
	}
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

func newTestSharedLister(pods []*v1.Pod, nodes []*v1.Node) *testSharedLister {
	nodeInfoMap := make(map[string]*framework.NodeInfo)
	nodeInfos := make([]*framework.NodeInfo, 0)
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeInfoMap[nodeName]; !ok {
			nodeInfoMap[nodeName] = framework.NewNodeInfo()
		}
		nodeInfoMap[nodeName].AddPod(pod)
	}
	for _, node := range nodes {
		if _, ok := nodeInfoMap[node.Name]; !ok {
			nodeInfoMap[node.Name] = framework.NewNodeInfo()
		}
		nodeInfoMap[node.Name].SetNode(node)
	}

	for _, v := range nodeInfoMap {
		nodeInfos = append(nodeInfos, v)
	}

	return &testSharedLister{
		nodes:       nodes,
		nodeInfos:   nodeInfos,
		nodeInfoMap: nodeInfoMap,
	}
}

func getNodes(nodesNum int64, regionNames []string, zoneNames []string) (nodes []*v1.Node) {
	nodeResources := map[v1.ResourceName]string{
		v1.ResourceCPU:    "8000m",
		v1.ResourceMemory: "16Gi",
	}
	var i int64
	for i = 0; i < nodesNum; i++ {
		regionId := randomInt(0, len(regionNames))
		zoneId := randomInt(0, len(zoneNames))
		region := regionNames[regionId]
		zone := zoneNames[zoneId]

		nodes = append(nodes, st.MakeNode().Name(
			fmt.Sprintf("n-%v", (i+1))).Label(v1.LabelTopologyRegion, region).Label(v1.LabelTopologyZone, zone).Capacity(nodeResources).Obj())
	}
	return nodes
}

func randomInt(min int, max int) int {
	return min + rand.Intn(max-min)
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

func makePodAllocated(selector string, podName string, hostname string, priority int32, appGroup string, requests, limits v1.ResourceList) *v1.Pod {
	label := make(map[string]string)
	label[agv1alpha1.AppGroupLabel] = appGroup
	label[agv1alpha1.AppGroupSelectorLabel] = selector

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: label,
		},
		Spec: v1.PodSpec{
			NodeName: hostname,
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
