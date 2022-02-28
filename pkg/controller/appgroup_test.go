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

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/controller"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	agfake "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	schedinformer "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
)

func TestAppGroupController_Run(t *testing.T) {
	ctx := context.TODO()

	// AppGroup: basic
	basicAppGroup := v1alpha1.AppGroupWorkloadList{
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P1", Selector: "P1", APIVersion: "apps/v1", Namespace: "default"},
			Dependencies: v1alpha1.DependenciesList{v1alpha1.DependenciesInfo{
				Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}}}},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"},
			Dependencies: v1alpha1.DependenciesList{v1alpha1.DependenciesInfo{
				Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}}}},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}},
	}

	basicTopologyOrder := v1alpha1.AppGroupTopologyList{
		v1alpha1.AppGroupTopologyInfo{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P1", Selector: "P1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
		v1alpha1.AppGroupTopologyInfo{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
		v1alpha1.AppGroupTopologyInfo{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
	}

	// AppGroup: OnlineBoutique
	onlineBoutique := v1alpha1.AppGroupWorkloadList{
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P1", Selector: "P1", APIVersion: "apps/v1", Namespace: "default"}, // frontend
			Dependencies: v1alpha1.DependenciesList{
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P4", Selector: "P4", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P6", Selector: "P6", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P8", Selector: "P8", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P9", Selector: "P9", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P10", Selector: "P10", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}, // cartService
			Dependencies: v1alpha1.DependenciesList{
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P11", Selector: "P11", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}, // productCatalogService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P4", Selector: "P4", APIVersion: "apps/v1", Namespace: "default"}, // currencyService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P5", Selector: "P5", APIVersion: "apps/v1", Namespace: "default"}, // paymentService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P6", Selector: "P6", APIVersion: "apps/v1", Namespace: "default"}, // shippingService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P7", Selector: "P7", APIVersion: "apps/v1", Namespace: "default"}, // emailService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P8", Selector: "P8", APIVersion: "apps/v1", Namespace: "default"}, // checkoutService
			Dependencies: v1alpha1.DependenciesList{
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P4", Selector: "P4", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P5", Selector: "P5", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P6", Selector: "P6", APIVersion: "apps/v1", Namespace: "default"}},
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P7", Selector: "P7", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P9", Selector: "P9", APIVersion: "apps/v1", Namespace: "default"}, // recommendationService
			Dependencies: v1alpha1.DependenciesList{
				v1alpha1.DependenciesInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}},
			}},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P10", Selector: "P10", APIVersion: "apps/v1", Namespace: "default"}, // adService
		},
		v1alpha1.AppGroupWorkload{
			Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P11", Selector: "P11", APIVersion: "apps/v1", Namespace: "default"}, // redis-cart
		},
	}

	onlineBoutiqueTopologyOrderKahn := v1alpha1.AppGroupTopologyList{
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P1", Selector: "P1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P10", Selector: "P10", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P9", Selector: "P9", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P8", Selector: "P8", APIVersion: "apps/v1", Namespace: "default"}, Index: 4},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P7", Selector: "P7", APIVersion: "apps/v1", Namespace: "default"}, Index: 5},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P6", Selector: "P6", APIVersion: "apps/v1", Namespace: "default"}, Index: 6},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P5", Selector: "P5", APIVersion: "apps/v1", Namespace: "default"}, Index: 7},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P4", Selector: "P4", APIVersion: "apps/v1", Namespace: "default"}, Index: 8},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}, Index: 9},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}, Index: 10},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P11", Selector: "P11", APIVersion: "apps/v1", Namespace: "default"}, Index: 11},
	}

	onlineBoutiqueTopologyOrderAlternateKahn := v1alpha1.AppGroupTopologyList{
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P1", Selector: "P1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P10", Selector: "P10", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P9", Selector: "P9", APIVersion: "apps/v1", Namespace: "default"}, Index: 5},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P8", Selector: "P8", APIVersion: "apps/v1", Namespace: "default"}, Index: 7},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P7", Selector: "P7", APIVersion: "apps/v1", Namespace: "default"}, Index: 9},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P6", Selector: "P6", APIVersion: "apps/v1", Namespace: "default"}, Index: 11},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P5", Selector: "P5", APIVersion: "apps/v1", Namespace: "default"}, Index: 10},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P4", Selector: "P4", APIVersion: "apps/v1", Namespace: "default"}, Index: 8},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P3", Selector: "P3", APIVersion: "apps/v1", Namespace: "default"}, Index: 6},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P2", Selector: "P2", APIVersion: "apps/v1", Namespace: "default"}, Index: 4},
		v1alpha1.AppGroupTopologyInfo{Workload: v1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "P11", Selector: "P11", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
	}

	cases := []struct {
		name                     string
		agName                   string
		numMembers               int32
		selectors                []string
		deploymentNames          []string
		podPhase                 v1.PodPhase
		podNextPhase             v1.PodPhase
		topologySortingAlgorithm string
		workloadList             v1alpha1.AppGroupWorkloadList
		desiredRunningWorkloads  int32
		desiredTopologyOrder     v1alpha1.AppGroupTopologyList
		appGroupCreateTime       *metav1.Time
	}{
		{
			name:                     "AppGroup running: Simple chain with 3 pods",
			agName:                   "simpleChain",
			numMembers:               3,
			selectors:                []string{"P1", "P2", "P3"},
			deploymentNames:          []string{"P1", "P2", "P3"},
			desiredRunningWorkloads:  3,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: v1alpha1.AppGroupKahnSort,
			workloadList:             basicAppGroup,
			desiredTopologyOrder:     basicTopologyOrder,
		},
		{
			name:                     "AppGroup Online Boutique - KahnSort - https://github.com/GoogleCloudPlatform/microservices-demo",
			agName:                   "onlineBoutique",
			numMembers:               11,
			selectors:                []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P11"},
			deploymentNames:          []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P11"},
			desiredRunningWorkloads:  11,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: v1alpha1.AppGroupKahnSort,
			workloadList:             onlineBoutique,
			desiredTopologyOrder:     onlineBoutiqueTopologyOrderKahn,
		},
		{
			name:                     "AppGroup Online Boutique - AlternateKahn - https://github.com/GoogleCloudPlatform/microservices-demo",
			agName:                   "onlineBoutique",
			numMembers:               11,
			selectors:                []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P11"},
			deploymentNames:          []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P11"},
			desiredRunningWorkloads:  11,
			podPhase:                 v1.PodRunning,
			topologySortingAlgorithm: v1alpha1.AppGroupAlternateKahn,
			workloadList:             onlineBoutique,
			desiredTopologyOrder:     onlineBoutiqueTopologyOrderAlternateKahn,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := makePodsAppGroup(c.selectors, c.deploymentNames, c.agName, c.podPhase)

			var kubeClient = fake.NewSimpleClientset()

			if len(ps) == 3 {
				kubeClient = fake.NewSimpleClientset(ps[0], ps[1], ps[2])
			} else if len(ps) == 11 {
				kubeClient = fake.NewSimpleClientset(ps[0], ps[1], ps[2], ps[3], ps[4], ps[5], ps[6], ps[7], ps[8], ps[9], ps[10])
			}

			ag := makeAG(c.agName, c.numMembers, c.topologySortingAlgorithm, c.workloadList, c.appGroupCreateTime)
			agClient := agfake.NewSimpleClientset(ag)

			informerFactory := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
			agInformerFactory := schedinformer.NewSharedInformerFactory(agClient, controller.NoResyncPeriodFunc())
			podInformer := informerFactory.Core().V1().Pods()
			agInformer := agInformerFactory.Scheduling().V1alpha1().AppGroups()

			ctrl := NewAppGroupController(kubeClient, agInformer, podInformer, agClient)

			agInformerFactory.Start(ctx.Done())
			informerFactory.Start(ctx.Done())

			go ctrl.Run(1, ctx.Done())
			err := wait.Poll(200*time.Millisecond, 1*time.Second, func() (done bool, err error) {
				ag, err := agClient.SchedulingV1alpha1().AppGroups("default").Get(ctx, c.agName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if ag.Status.RunningWorkloads == 0 {
					return false, fmt.Errorf("want %v, got %v", c.desiredRunningWorkloads, ag.Status.RunningWorkloads)
				}
				if ag.Status.TopologyOrder == nil {
					return false, fmt.Errorf("want %v, got %v", c.desiredTopologyOrder, ag.Status.TopologyOrder)
				}
				for _, pod := range ag.Status.TopologyOrder {
					for _, desiredWorkload := range c.desiredTopologyOrder {
						if desiredWorkload.Workload.Name == pod.Workload.Name {
							if pod.Index != desiredWorkload.Index { // Some algorithms might give a different result depending on the service topology
								return false, fmt.Errorf("want %v, got %v", desiredWorkload.Index, pod.Index)
							}
						}
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatal("Unexpected error", err)
			}
		})
	}
}

func makePodsAppGroup(selectors []string, podNames []string, agName string, phase v1.PodPhase) []*v1.Pod {
	pds := make([]*v1.Pod, 0)
	i := 0
	for id, name := range podNames {
		pod := st.MakePod().Namespace("default").Name(name + fmt.Sprint(i)).Obj()
		pod.Labels = map[string]string{v1alpha1.AppGroupLabel: agName, v1alpha1.AppGroupSelectorLabel: selectors[id]}
		pod.Status.Phase = phase
		pds = append(pds, pod)
		i += i
	}
	return pds
}

func makeAG(agName string, numMembers int32, topologySortingAlgorithm string, appGroupWorkload v1alpha1.AppGroupWorkloadList, createTime *metav1.Time) *v1alpha1.AppGroup {
	ag := &v1alpha1.AppGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              agName,
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: v1alpha1.AppGroupSpec{
			NumMembers:               numMembers,
			TopologySortingAlgorithm: topologySortingAlgorithm,
			Workloads:                appGroupWorkload,
		},
		Status: v1alpha1.AppGroupStatus{
			RunningWorkloads:  0,
			ScheduleStartTime: metav1.Time{Time: time.Now()},
			TopologyOrder:     nil,
		},
	}
	if createTime != nil {
		ag.CreationTimestamp = *createTime
	}
	return ag
}
