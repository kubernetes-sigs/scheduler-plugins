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

package integration

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/networkaware/topologicalsort"
	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"
	"sigs.k8s.io/scheduler-plugins/test/util"

	appgroupapi "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup"
	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
)

func TestTopologicalSortPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agv1alpha1.AddToScheme(scheme))

	client, err := ctrlclient.New(globalKubeConfig, ctrlclient.Options{Scheme: scheme})

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == appgroupapi.GroupName {
				t.Log("The AppGroup CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for AppGroup CRD to be ready: %v", err)
	}

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: topologicalsort.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: topologicalsort.Name,
		Args: &scheconfig.TopologicalSortArgs{
			Namespaces: []string{ns},
		},
	})

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{topologicalsort.Name: topologicalsort.New}),
	)
	syncInformerFactory(testCtx)
	// Do not start the scheduler.
	// go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name("fake-node").Label("node", nodeName).Obj()
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(500, resource.DecimalSI),
	}
	node, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
	t.Logf("Node created: %v", node)
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}

	// Create AppGroup CRD: basic
	basicAppGroup := MakeAppGroup(ns, "basic").Spec(
		agv1alpha1.AppGroupSpec{
			NumMembers:               3,
			TopologySortingAlgorithm: "KahnSort",
			Workloads: agv1alpha1.AppGroupWorkloadList{
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"},
					Dependencies: agv1alpha1.DependenciesList{agv1alpha1.DependenciesInfo{
						Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}}}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"},
					Dependencies: agv1alpha1.DependenciesList{agv1alpha1.DependenciesInfo{
						Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}}}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
	).Status(agv1alpha1.AppGroupStatus{
		RunningWorkloads:        3,
		ScheduleStartTime:       metav1.Now(),
		TopologyCalculationTime: metav1.Now(),
		TopologyOrder: agv1alpha1.AppGroupTopologyList{
			agv1alpha1.AppGroupTopologyInfo{
				Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
			agv1alpha1.AppGroupTopologyInfo{
				Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
			agv1alpha1.AppGroupTopologyInfo{
				Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
		},
	},
	).Obj()

	// Create AppGroup CRD: onlineboutique
	onlineboutiqueAppGroup := MakeAppGroup(ns, "onlineboutique").Spec(
		agv1alpha1.AppGroupSpec{
			NumMembers:               3,
			TopologySortingAlgorithm: "KahnSort",
			Workloads: agv1alpha1.AppGroupWorkloadList{
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, // frontend
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, // cartService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, // productCatalogService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}, // currencyService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}, // paymentService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}, // shippingService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}, // emailService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}, // checkoutService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}},
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}},
					},
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}, // recommendationService
					Dependencies: agv1alpha1.DependenciesList{
						agv1alpha1.DependenciesInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
					}},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}, // adService
				},
				agv1alpha1.AppGroupWorkload{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}, // redis-cart
				},
			},
		},
	).Status(agv1alpha1.AppGroupStatus{
		RunningWorkloads:        3,
		ScheduleStartTime:       metav1.Now(),
		TopologyCalculationTime: metav1.Now(),
		TopologyOrder: agv1alpha1.AppGroupTopologyList{
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"}, Index: 1},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p10", Selector: "p10", APIVersion: "apps/v1", Namespace: "default"}, Index: 2},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p9", Selector: "p9", APIVersion: "apps/v1", Namespace: "default"}, Index: 3},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p8", Selector: "p8", APIVersion: "apps/v1", Namespace: "default"}, Index: 4},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p7", Selector: "p7", APIVersion: "apps/v1", Namespace: "default"}, Index: 5},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p6", Selector: "p6", APIVersion: "apps/v1", Namespace: "default"}, Index: 6},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p5", Selector: "p5", APIVersion: "apps/v1", Namespace: "default"}, Index: 7},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p4", Selector: "p4", APIVersion: "apps/v1", Namespace: "default"}, Index: 8},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}, Index: 9},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}, Index: 10},
			agv1alpha1.AppGroupTopologyInfo{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p11", Selector: "p11", APIVersion: "apps/v1", Namespace: "default"}, Index: 11},
		},
	},
	).Obj()

	// Sort Topology order in AppGroup CR
	sort.Sort(networkawareutil.ByWorkloadSelector(basicAppGroup.Status.TopologyOrder))
	sort.Sort(networkawareutil.ByWorkloadSelector(onlineboutiqueAppGroup.Status.TopologyOrder))

	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name     string
		pods     []*v1.Pod
		appGroup []*agv1alpha1.AppGroup
		podNames []string
	}{
		{
			name: "basic-appGroup-compare-p1-p2",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p2-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{basicAppGroup},
			podNames: []string{"p1-test-1", "p2-test-1"},
		},
		{
			name: "basic-appGroup-compare-p1-p3",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p3-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{basicAppGroup},
			podNames: []string{"p1-test-1", "p3-test-1"},
		},
		{
			name: "basic-appGroup-compare-p2-p3",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p2-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p3-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{basicAppGroup},
			podNames: []string{"p2-test-1", "p3-test-1"},
		},
		{
			name: "basic-appGroup-compare-p1-p2-p3",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p2-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p3-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{basicAppGroup},
			podNames: []string{"p1-test-1", "p2-test-1", "p3-test-1"},
		},
		{
			name: "onlineboutique-appGroup-compare-p1-p5",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p5-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p5").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{onlineboutiqueAppGroup},
			podNames: []string{"p1-test-1", "p5-test-1"},
		},
		{
			name: "onlineboutique-appGroup-compare-p4-p8",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p4-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p4").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p8-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p8").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{onlineboutiqueAppGroup},
			podNames: []string{"p8-test-1", "p4-test-1"},
		},
		{
			name: "onlineboutique-appGroup-compare-p1-p2-p3-p4-p5-p6-p7-p8-p9-p10-p11",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p2-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p3-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p4-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p4").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p5-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p5").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p6-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p6").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p7-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p7").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p8-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p8").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p9-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p9").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p10-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p10").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p11-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "onlineboutique").Label(agv1alpha1.AppGroupSelectorLabel, "p11").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroup: []*agv1alpha1.AppGroup{onlineboutiqueAppGroup},
			podNames: []string{"p1-test-1", "p10-test-1", "p9-test-1", "p8-test-1", "p7-test-1", "p6-test-1",
				"p5-test-1", "p4-test-1", "p3-test-1", "p2-test-1", "p11-test-1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start topologicalSort integration test %v ...", tt.name)
			defer cleanupAppGroups(testCtx.Ctx, client, tt.appGroup)
			defer cleanupPods(t, testCtx, tt.pods)

			// create AppGroup
			t.Logf("Step 1 - Start by creating the basic appGroup...")
			if err := createAppGroups(testCtx.Ctx, client, tt.appGroup); err != nil {
				t.Fatal(err)
			}

			// Create Pods
			t.Logf("Step 2 -  Create the pods...")
			for _, p := range tt.pods {
				t.Logf("Pod %q created", p.Name)
				_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", p.Name, err)
				}
			}

			// Wait for all Pods are in the scheduling queue.
			t.Logf("Step 3 -  Wait for pods being in the scheduling queue....")
			err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*200, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
				pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
				if len(pendingPods) == len(tt.pods) {
					return true, nil
				}
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			// Expect Pods are popped as in the TopologyOrder defined by the AppGroup.
			t.Logf("Step 4 -  Expect pods to be popped out according to the topologicalSort plugin...")
			logger := klog.FromContext(testCtx.Ctx)
			for i := 0; i < len(tt.podNames); i++ {
				podInfo, _ := testCtx.Scheduler.NextPod(logger)
				if podInfo.Pod.Name != tt.podNames[i] {
					t.Errorf("Expect Pod %q, but got %q", tt.podNames[i], podInfo.Pod.Name)
				} else {
					t.Logf("Pod %q is popped out as expected.", podInfo.Pod.Name)
				}
			}
		})
	}
}
