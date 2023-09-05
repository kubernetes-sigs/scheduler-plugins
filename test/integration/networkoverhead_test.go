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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/networkaware/networkoverhead"
	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"
	"sigs.k8s.io/scheduler-plugins/test/util"

	appgroupapi "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup"
	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntapi "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
)

func TestNetworkOverheadPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agv1alpha1.AddToScheme(scheme))
	utilruntime.Must(ntv1alpha1.AddToScheme(scheme))

	client, err := ctrlclient.New(globalKubeConfig, ctrlclient.Options{Scheme: scheme})

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
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

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == ntapi.GroupName {
				t.Log("The NetworkTopology CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for Network Topology CRD to be ready: %v", err)
	}

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}

	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled, schedapi.Plugin{Name: networkoverhead.Name})
	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled, schedapi.Plugin{Name: networkoverhead.Name})
	cfg.Profiles[0].Plugins.Score.Enabled = append(cfg.Profiles[0].Plugins.Score.Enabled, schedapi.Plugin{Name: networkoverhead.Name})

	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: networkoverhead.Name,
		Args: &scheconfig.NetworkOverheadArgs{
			Namespaces:          []string{ns},
			WeightsName:         "UserDefined",
			NetworkTopologyName: "nt-test",
		},
	})

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{networkoverhead.Name: networkoverhead.New}),
	)

	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create nodes
	resList := map[v1.ResourceName]string{
		v1.ResourceCPU:    "64",
		v1.ResourceMemory: "128Gi",
		v1.ResourcePods:   "32",
		hugepages2Mi:      "896Mi",
		nicResourceName:   "48",
	}

	// add region and zone labels
	regionLabels := []string{"us-west-1", "us-west-1", "us-west-1", "us-west-1",
		"us-east-1", "us-east-1", "us-east-1", "us-east-1"}
	zoneLabels := []string{"Z1", "Z1", "Z2", "Z2", "Z3", "Z3", "Z4", "Z4"}
	nodeNames := []string{"fake-n1", "fake-n2", "fake-n3", "fake-n4",
		"fake-n5", "fake-n6", "fake-n7", "fake-n8"}
	var nodes []*v1.Node

	for i, nodeName := range nodeNames {
		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).
			Label(v1.LabelTopologyRegion, regionLabels[i]).Label(v1.LabelTopologyZone, zoneLabels[i]).Capacity(resList).Obj()
		_, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
		nodes = append(nodes, newNode)
	}

	nodeList, err := cs.CoreV1().Nodes().List(testCtx.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to create NodeList %q: %v", nodeList, err)
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
		RunningWorkloads:  3,
		ScheduleStartTime: metav1.Time{time.Now()}, TopologyCalculationTime: metav1.Time{time.Now()},
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

	// Sort Topology order in AppGroup CR
	sort.Sort(networkawareutil.ByWorkloadSelector(basicAppGroup.Status.TopologyOrder))

	// Create Network Topology CR: nt-test
	networkTopology := MakeNetworkTopology(ns, "nt-test").Spec(
		ntv1alpha1.NetworkTopologySpec{
			Weights: ntv1alpha1.WeightList{
				ntv1alpha1.WeightInfo{Name: "UserDefined", TopologyList: ntv1alpha1.TopologyList{
					ntv1alpha1.TopologyInfo{
						TopologyKey: "topology.kubernetes.io/region",
						OriginList: ntv1alpha1.OriginList{
							ntv1alpha1.OriginInfo{Origin: "us-west-1", CostList: []ntv1alpha1.CostInfo{{Destination: "us-east-1", NetworkCost: 20}}},
							ntv1alpha1.OriginInfo{Origin: "us-east-1", CostList: []ntv1alpha1.CostInfo{{Destination: "us-west-1", NetworkCost: 20}}},
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
				}},
			},
			ConfigmapName: "netperf-metrics",
		}).Status(ntv1alpha1.NetworkTopologyStatus{}).Obj()

	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name              string
		pods              []*v1.Pod
		appGroups         []*agv1alpha1.AppGroup
		networkTopologies []*ntv1alpha1.NetworkTopology
		podNames          []string
		expectedNodes     []string
	}{
		{
			name: "basic-appGroup-deploy-three-pods-p1-p2-p3",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("p1-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p2-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("p3-test-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(agv1alpha1.AppGroupLabel, "basic").Label(agv1alpha1.AppGroupSelectorLabel, "p3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			appGroups:         []*agv1alpha1.AppGroup{basicAppGroup},
			networkTopologies: []*ntv1alpha1.NetworkTopology{networkTopology},
			podNames:          []string{"p1-test-1", "p2-test-1", "p3-test-1"},
			expectedNodes:     nodeNames,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start NetworkOverhead integration test %v ...", tt.name)
			defer cleanupAppGroups(testCtx.Ctx, client, tt.appGroups)
			defer cleanupNetworkTopologies(testCtx.Ctx, client, tt.networkTopologies)
			defer cleanupPods(t, testCtx, tt.pods)

			// create AppGroup
			t.Logf("Step 1 - Start by creating the basic appGroup...")
			if err := createAppGroups(testCtx.Ctx, client, tt.appGroups); err != nil {
				t.Fatal(err)
			}

			// create NetworkTopology
			t.Logf("Step 2 - create the nt CR...")
			if err := createNetworkTopologies(testCtx.Ctx, client, tt.networkTopologies); err != nil {
				t.Fatal(err)
			}

			// Create Pods
			t.Logf("Step 3 -  Create the pods...")
			for _, p := range tt.pods {
				t.Logf("Creating Pod %q", p.Name)
				_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", p.Name, err)
				}
			}

			t.Logf("Step 4 -  Deploy the pods...")
			for _, p := range tt.pods {
				if len(tt.expectedNodes) > 0 {
					// Wait for the pod to be scheduled.
					if err := wait.Poll(1*time.Second, 20*time.Second, func() (bool, error) {
						return podScheduled(cs, ns, p.Name), nil

					}); err != nil {
						t.Errorf("pod %q to be scheduled, error: %v", p.Name, err)
					}

					t.Logf("Pod %v scheduled", p.Name)
					// The other pods should be scheduled on the small nodes.
					nodeName, err := getNodeName(testCtx.Ctx, cs, ns, p.Name)
					if err != nil {
						t.Log(err)
					}
					if contains(tt.expectedNodes, nodeName) {
						t.Logf("Pod %q is on the expected node %s.", p.Name, nodeName)
					} else {
						t.Errorf("Pod %s is expected on node %s, but found on node %s",
							p.Name, tt.expectedNodes, nodeName)
					}
				}
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}
