/*
Copyright 2021 The Kubernetes Authors.

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
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clientset "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
	"sigs.k8s.io/scheduler-plugins/test/util"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

const (
	testPodName = "topology-aware-scheduler-pod"
)

var (
	mostAllocatedScheduler      = fmt.Sprintf("%v-scheduler", string(scheconfig.MostAllocated))
	balancedAllocationScheduler = fmt.Sprintf("%v-scheduler", string(scheconfig.BalancedAllocation))
	leastAllocatedScheduler     = fmt.Sprintf("%v-scheduler", string(scheconfig.LeastAllocated))
	leastNUMAScheduler          = fmt.Sprintf("%v-scheduler", string(scheconfig.LeastNUMANodes))
)

// should be filled by the user
type nrtTestUserEntry struct {
	// description contains the test type and tier as described in noderesourcetopology/TESTS.md
	// and a short description of the test itself
	description string
	initCntReq  []map[string]string
	cntReq      []map[string]string
	errMsg      string
	// this testing batch is going to be run against the same node and NRT objects, hence we're not specifying them.
	isBurstable   bool
	expectedNodes []string
}

type nrtTestEntry struct {
	name                   string
	pods                   []*v1.Pod
	nodeResourceTopologies []*topologyv1alpha2.NodeResourceTopology
	expectedNodes          []string
	errMsg                 string
}

func TestTopologyMatchPluginValidation(t *testing.T) {
	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].Plugins.Score.Enabled = append(cfg.Profiles[0].Plugins.Score.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: noderesourcetopology.Name,
		Args: &scheconfig.NodeResourceTopologyMatchArgs{
			ScoringStrategy: scheconfig.ScoringStrategy{Type: "incorrect"},
		},
	})

	cs := clientset.NewForConfigOrDie(globalKubeConfig)
	informer := scheduler.NewInformerFactory(cs, 0)
	dynInformerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamic.NewForConfigOrDie(globalKubeConfig), 0, v1.NamespaceAll, nil)
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: cs.EventsV1(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = scheduler.New(
		ctx,
		cs,
		informer,
		dynInformerFactory,
		profile.NewRecorderFactory(eventBroadcaster),
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{noderesourcetopology.Name: noderesourcetopology.New}),
	)

	if err == nil {
		t.Error("expected error not to be nil")
	}

	errMsg := "scoringStrategy.type: Invalid value:"
	if !strings.Contains(err.Error(), errMsg) {
		t.Errorf("expected error to contain: %s in error message: %s", errMsg, err.Error())
	}
}

func TestTopologyMatchPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := clientset.NewForConfigOrDie(globalKubeConfig)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(topologyv1alpha2.AddToScheme(scheme))
	extClient, err := ctrlclient.New(globalKubeConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == "topology.node.k8s.io" {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].Plugins.Score.Enabled = append(cfg.Profiles[0].Plugins.Score.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: noderesourcetopology.Name,
		Args: &scheconfig.NodeResourceTopologyMatchArgs{
			ScoringStrategy: scheconfig.ScoringStrategy{Type: scheconfig.MostAllocated},
		},
	})
	cfg.Profiles = append(cfg.Profiles,
		// a profile with both the filter and score enabled and score strategy is MostAllocated
		makeProfileByPluginArgs(
			mostAllocatedScheduler,
			makeResourceAllocationScoreArgs(&scheconfig.ScoringStrategy{Type: scheconfig.MostAllocated}),
		),
		// a profile with both the filter and score enabled and score strategy is BalancedAllocation
		makeProfileByPluginArgs(
			balancedAllocationScheduler,
			makeResourceAllocationScoreArgs(&scheconfig.ScoringStrategy{Type: scheconfig.BalancedAllocation}),
		),
		// a profile with both the filter and score enabled and score strategy is LeastAllocated
		makeProfileByPluginArgs(
			leastAllocatedScheduler,
			makeResourceAllocationScoreArgs(&scheconfig.ScoringStrategy{Type: scheconfig.LeastAllocated}),
		),
		// a profile with both the filter and score enabled and score strategy is LeastNUMANodes
		makeProfileByPluginArgs(
			leastNUMAScheduler,
			makeResourceAllocationScoreArgs(&scheconfig.ScoringStrategy{Type: scheconfig.LeastNUMANodes}),
		),
	)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{noderesourcetopology.Name: noderesourcetopology.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create a Node.
	resList := map[v1.ResourceName]string{
		v1.ResourceCPU:              "64",
		v1.ResourceMemory:           "128Gi",
		v1.ResourcePods:             "32",
		hugepages2Mi:                "896Mi",
		nicResourceName:             "48",
		v1.ResourceEphemeralStorage: "32Gi",
	}
	for _, nodeName := range []string{"fake-node-1", "fake-node-2"} {
		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).Capacity(resList).Obj()
		n, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}

		t.Logf(" Node %s created: %s", nodeName, formatObject(n))
	}

	nodeList, err := cs.CoreV1().Nodes().List(testCtx.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("can't list nodes: %s", err.Error())
	}

	t.Logf("NodeList: %v", nodeList)
	pause := imageutils.GetPauseImageName()
	tests := []nrtTestEntry{
		{
			name: "Filtering out nodes that cannot fit resources on a single numa node in case of Guaranteed pod",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName), map[string]string{cpu: "4", memory: "5Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "0", "0"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling of a burstable pod requesting only cpus",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name(testPodName).Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "0", "0"),
						}).Obj(),

				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling of a burstable pod requesting only memory",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name(testPodName).Req(map[v1.ResourceName]string{v1.ResourceMemory: "5Gi"}).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "none",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with most-allocated strategy scheduler",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(mostAllocatedScheduler),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(balancedAllocationScheduler),
					map[string]string{cpu: "2", memory: "2Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with least-allocated strategy scheduler",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastAllocatedScheduler),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1"},
		},
		// in the following "Scheduling Best-Effort pod with most-allocated/balanced-allocation/least-allocated strategy" tests,
		// both nodes are expected to get the maximum score.
		// in case of multiple nodes with the same highest score, the chosen node selection might vary.
		// thus, we can't tell exactly which node will get selected, so the only option is to accept both of them,
		// and it's still good enough for making sure that pod not stuck in pending state and/or the program isn't crashing
		{
			name: "Scheduling Best-Effort pod with most-allocated strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(mostAllocatedScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(balancedAllocationScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with least-allocated strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastAllocatedScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "SingleNUMANodePodLevel: Filtering out nodes that cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		// the following tests are different from the NodeResourceFit tests in the sense that
		// they would pass the NodeResourceFit since there are enough resources for the pod to be fit
		// at the node level, but not at the NUMA node level
		{
			name: "SingleNUMANodeContainerLevel: Filtering out nodes that cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "3", memory: "5Gi"}, false),
					map[string]string{cpu: "3", memory: "5Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "8", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "SingleNUMANodeContainerLevel: Filtering out nodes that cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "4Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "4", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1"},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "10Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "10Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "4", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "6Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "single-numa-node",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
		},
		{
			name: "Scheduling Guaranteed pod with LeastNUMANodes strategy scheduler, pod scope",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with LeastNUMANodes strategy scheduler, one node only non NUMA resources",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha2.BestEffortPodLevel).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha2.BestEffortPodLevel).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with LeastNUMANodes strategy scheduler, different allocation pod scope",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{cpu: "4", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha2.BestEffortPodLevel).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha2.BestEffortPodLevel).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with LeastNUMANodes strategy scheduler, different allocation pod scope",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{cpu: "4", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with init container with LeastNUMANodes strategy scheduler and pod scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "6Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed multi container pod with LeastNUMANodes strategy scheduler and pod scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},

			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed multi container pod with LeastNUMANodes strategy scheduler and container scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "6Gi"}, false).Obj(),
			},

			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with init container with LeastNUMANodes strategy scheduler and container scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "6Gi"}, true).Obj(),
			},

			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with multiple containers with LeastNUMANodes strategy scheduler, different resource allocation, pod scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "3", memory: "4Gi"}, false),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with multiple containers with LeastNUMANodes strategy scheduler, different resource allocation, container scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "3", memory: "4Gi"}, false),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "container",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed multiple containers pod with LeastNUMANodes strategy scheduler, can't fit one node, pod scope",
			pods: []*v1.Pod{
				util.WithLimits(
					util.WithLimits(
						st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
						map[string]string{cpu: "3", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "6Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").
					Attributes(topologyv1alpha2.AttributeList{
						{
							Name:  nodeconfig.AttributePolicy,
							Value: "best-effort",
						},
						{
							Name:  nodeconfig.AttributeScope,
							Value: "pod",
						},
					}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed single containers pod with LeastNUMANodes strategy scheduler, pod scope,one node non optimal, one no resources left",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{
						cpu: "4", memory: "4Gi", gpuResourceName: "2",
					}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha2.BestEffortPodLevel).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 10,
							},
							{
								Name:  "node-1",
								Value: 12,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "0", "0"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 12,
							},
							{
								Name:  "node-1",
								Value: 10,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 20,
							},
							{
								Name:  "node-1",
								Value: 20,
							},
							{
								Name:  "node-2",
								Value: 10,
							},
						},
					).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha2.BestEffortPodLevel).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "0", "0"),
						}).
					Zone(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "0", "0"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1"},
		},
		{
			name: "Scheduling Guaranteed single containers pod with LeastNUMANodes strategy scheduler, pod scope,one node non optimal, one node optimal",
			pods: []*v1.Pod{
				util.WithLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastNUMAScheduler),
					map[string]string{
						cpu: "4", memory: "4Gi", gpuResourceName: "2",
					}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha2.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha2.BestEffortPodLevel).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 10,
							},
							{
								Name:  "node-1",
								Value: 12,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "0", "0"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 12,
							},
							{
								Name:  "node-1",
								Value: 10,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 20,
							},
							{
								Name:  "node-1",
								Value: 20,
							},
							{
								Name:  "node-2",
								Value: 10,
							},
						},
					).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha2.BestEffortPodLevel).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 10,
							},
							{
								Name:  "node-1",
								Value: 12,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "1", "1"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 12,
							},
							{
								Name:  "node-1",
								Value: 10,
							},
							{
								Name:  "node-2",
								Value: 20,
							},
						},
					).
					ZoneWithCosts(
						topologyv1alpha2.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
							noderesourcetopology.MakeTopologyResInfo(gpuResourceName, "0", "0"),
						},
						topologyv1alpha2.CostList{
							{
								Name:  "node-0",
								Value: 20,
							},
							{
								Name:  "node-1",
								Value: 20,
							},
							{
								Name:  "node-2",
								Value: 10,
							},
						},
					).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
	}

	// a dedicated set of tests for the scope=container flow
	scopeEqualsContainerTests := []nrtTestUserEntry{
		{
			description: "[4][tier1] multi containers with good devices and hugepages allocation, spread across NUMAs - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "6Gi", hugepages2Mi: "500Mi", nicResourceName: "16"},
				{cpu: "2", memory: "6Gi", hugepages2Mi: "50Mi", nicResourceName: "8"},
			},
		},
		{
			description: "[5][tier1] multi containers with hugepages over allocation, spread across NUMAs - not fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "6Gi", hugepages2Mi: "400Mi"},
				{cpu: "2", memory: "6Gi", hugepages2Mi: "400Mi"},
			},
			errMsg: "cannot align container", // cnt-2
		},
		{
			description: "[5][tier1] multi containers with device over allocation, spread across NUMAs - not fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "6Gi", hugepages2Mi: "50Mi", nicResourceName: "20"},
				{cpu: "2", memory: "6Gi", hugepages2Mi: "500Mi", nicResourceName: "20"},
			},
			errMsg: "cannot align container", // cnt-2
		},
		{
			description: "[7][tier1] init container with cpu over allocation, multi-containers with good allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "40", memory: "40Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "1", memory: "4Gi"},
				{cpu: "1", memory: "4Gi"},
			},
			errMsg: "cannot align init container", // initcnt-1
		},
		{
			description: "[7][tier1] init container with memory over allocation, multi-containers with good allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "70Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "1", memory: "4Gi"},
				{cpu: "1", memory: "4Gi"},
			},
			errMsg: "cannot align init container", // initcnt-1
		},
		{
			description: "[11][tier1] init container with good allocation, multi-containers spread across NUMAs - fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
			},
		},
		{
			description: "[12][tier1] init container with good allocation, multi-containers with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "10Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[12][tier1] init container with good allocation, multi-containers with memory over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "2", memory: "40Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[17][tier1] multi init containers with good allocation, multi-containers spread across NUMAs - fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "6", memory: "10Gi"},
			},
		},
		{
			description: "[18][tier1] multi init containers with good allocation, multi-containers with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "10Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[18][tier1] multi init containers with good allocation, multi-containers with memory over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
				{cpu: "4", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "35Gi"},
				{cpu: "20", memory: "35Gi"},
				{cpu: "2", memory: "50Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[24][tier1] multi init containers with good allocation, multi-containers with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "6Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[24][tier1] multi init containers with good allocation, multi-containers with memory over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "35Gi"},
				{cpu: "20", memory: "35Gi"},
				{cpu: "2", memory: "50Gi"},
			},
			errMsg: "cannot align container", // cnt-3
		},
		{
			description: "[27][tier1] multi init containers with good allocation, container with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "35", memory: "40Gi"},
			},
			errMsg: "cannot align container", // cnt-1
		},
		{
			description: "[28][tier1] multi init containers with good allocation, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
			},
		},
		{
			description: "[29][tier1] multi init containers when sum of their cpus requests (together) is over allocatable, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
				{cpu: "30", memory: "10Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "2", memory: "6Gi"},
			},
		},
		{
			description: "[29][tier1] multi init containers when sum of their memory requests (together) is over allocatable, multi-containers with good allocation - fit",
			initCntReq: []map[string]string{
				{cpu: "3", memory: "50Gi"},
				{cpu: "3", memory: "50Gi"},
				{cpu: "3", memory: "50Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "20", memory: "40Gi"},
				{cpu: "2", memory: "6Gi"},
			},
		},
		{
			description: "[32][tier1] multi init containers with cpu over allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "40", memory: "50Gi"},
				{cpu: "3", memory: "50Gi"},
				{cpu: "3", memory: "50Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "2", memory: "6Gi"},
			},
			errMsg: "cannot align init container", // initcnt-1
		},
		{
			description: "[32][tier1] multi init containers with over memory allocation - not fit",
			initCntReq: []map[string]string{
				{cpu: "20", memory: "50Gi"},
				{cpu: "20", memory: "65Gi"},
				{cpu: "3", memory: "50Gi"},
			},
			cntReq: []map[string]string{
				{cpu: "20", memory: "40Gi"},
				{cpu: "2", memory: "6Gi"},
			},
			errMsg: "cannot align init container", // initcnt-2
		},
		// ephemeral storage
		{
			description: "[tier1] single containers one requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
			},
		},
		{
			description: "[tier1] multi containers all requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi", ephemeralStorage: "512Mi"},
				{cpu: "4", memory: "8Gi", ephemeralStorage: "2Gi"},
			},
		},
		{
			description: "[tier1] multi containers some requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi"},
				{cpu: "4", memory: "8Gi", ephemeralStorage: "2Gi"},
			},
		},
		{
			description: "[tier1] multi containers one requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi"},
				{cpu: "4", memory: "8Gi"},
			},
		},
		{
			description: "[tier1][burstable] single containers one requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
			},
			isBurstable:   true,
			expectedNodes: []string{"fake-node-1", "fake-node-2"}, // any node
		},
		{
			description: "[tier1][burstable] multi containers all requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi", ephemeralStorage: "512Mi"},
				{cpu: "4", memory: "8Gi", ephemeralStorage: "2Gi"},
			},
			isBurstable:   true,
			expectedNodes: []string{"fake-node-1", "fake-node-2"}, // any node
		},
		{
			description: "[tier1][burstable] multi containers some requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi"},
				{cpu: "4", memory: "8Gi", ephemeralStorage: "2Gi"},
			},
			isBurstable:   true,
			expectedNodes: []string{"fake-node-1", "fake-node-2"}, // any node
		},
		{
			description: "[tier1][burstable] multi containers one requiring ephemeral storage with good allocation - fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "4Gi", ephemeralStorage: "1Gi"},
				{cpu: "2", memory: "4Gi"},
				{cpu: "4", memory: "8Gi"},
			},
			isBurstable:   true,
			expectedNodes: []string{"fake-node-1", "fake-node-2"}, // any node
		},
	}
	tests = append(tests, parseTestUserEntry(scopeEqualsContainerTests, ns)...)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-topology-match-test %v", tt.name)
			defer cleanupNodeResourceTopologies(testCtx.Ctx, extClient, tt.nodeResourceTopologies)
			defer cleanupPods(t, testCtx, tt.pods)

			if err := createNodeResourceTopologies(testCtx.Ctx, extClient, tt.nodeResourceTopologies); err != nil {
				t.Fatal(err)
			}

			// Create Pods
			for _, p := range tt.pods {
				t.Logf("Creating Pod %q", p.Name)
				_, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", p.Name, err)
				}
			}

			for _, p := range tt.pods {
				if len(tt.expectedNodes) > 0 {
					// Wait for the pod to be scheduled.
					if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
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
						t.Logf("Pod %q is on a nodes as expected.", p.Name)
					} else {
						t.Errorf("Pod %s is expected on node %s, but found on node %s",
							p.Name, tt.expectedNodes, nodeName)
					}
					// if tt.expectedNodes == 0 we don't expect the pod to get scheduled
				} else {
					// wait for the pod scheduling to failed.
					var err error
					var events []v1.Event
					if err := wait.PollUntilContextTimeout(testCtx.Ctx, 5*time.Second, 20*time.Second, false, func(ctx context.Context) (bool, error) {
						events, err = getPodEvents(cs, ns, p.Name)
						if err != nil {
							// This could be a connection error, so we want to retry.
							klog.ErrorS(err, "Failed check pod scheduling status for pod", "pod", klog.KRef(ns, p.Name))
							return false, nil
						}
						candidateEvents := filterPodFailedSchedulingEvents(events)
						for _, ce := range candidateEvents {
							if strings.Contains(ce.Message, tt.errMsg) {
								return true, nil
							}
							klog.Warningf("Pod failed but error message does not contain substring: %q; got %q instead", tt.errMsg, ce.Message)
						}
						return false, nil
					}); err != nil {
						// we need more context to troubleshoot, but let's not clutter the actual error
						t.Logf("pod %q scheduling should failed with error: %v got %v events:\n%s", p.Name, tt.errMsg, err, formatEvents(events))
						t.Errorf("pod %q scheduling should failed, error: %v", p.Name, err)
					}
				}
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func makeProfileByPluginArgs(
	name string,
	args *scheconfig.NodeResourceTopologyMatchArgs,
) schedapi.KubeSchedulerProfile {
	return schedapi.KubeSchedulerProfile{
		SchedulerName: name,
		Plugins: &schedapi.Plugins{
			QueueSort: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: queuesort.Name},
				},
			},
			Filter: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: noderesourcetopology.Name},
				},
			},
			Score: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: noderesourcetopology.Name},
				},
			},
			Bind: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: defaultbinder.Name},
				},
			},
		},
		PluginConfig: []schedapi.PluginConfig{
			{
				Name: noderesourcetopology.Name,
				Args: args,
			},
		},
	}
}

func parseTestUserEntry(entries []nrtTestUserEntry, ns string) []nrtTestEntry {
	var teList []nrtTestEntry
	for i, e := range entries {
		desiredQoS := v1.PodQOSGuaranteed
		if e.isBurstable {
			desiredQoS = v1.PodQOSBurstable
		}

		p := st.MakePod().Name(fmt.Sprintf("%s-%d", testPodName, i+1)).Namespace(ns)
		for _, req := range e.initCntReq {
			if desiredQoS == v1.PodQOSGuaranteed {
				p = util.WithLimits(p, req, true)
			} else {
				p = util.WithRequests(p, req, true)
			}
		}

		for _, req := range e.cntReq {
			if desiredQoS == v1.PodQOSGuaranteed {
				p = util.WithLimits(p, req, false)
			} else {
				p = util.WithRequests(p, req, false)
			}
		}
		nodeTopologies := []*topologyv1alpha2.NodeResourceTopology{
			MakeNRT().Name("fake-node-1").
				Attributes(topologyv1alpha2.AttributeList{
					{
						Name:  nodeconfig.AttributePolicy,
						Value: "single-numa-node",
					},
					{
						Name:  nodeconfig.AttributeScope,
						Value: "container",
					},
				}).
				Zone(
					topologyv1alpha2.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "384Mi", "384Mi"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "16", "16")}).
				Zone(
					topologyv1alpha2.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "32"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "64Gi"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "512Mi", "512Mi"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "32", "32"),
					}).Obj(),
			// we set all available resource as 0
			// because we want all these tests to be running against fake-node-1
			MakeNRT().Name("fake-node-2").
				Attributes(topologyv1alpha2.AttributeList{
					{
						Name:  nodeconfig.AttributePolicy,
						Value: "single-numa-node",
					},
					{
						Name:  nodeconfig.AttributeScope,
						Value: "container",
					},
				}).
				Zone(
					topologyv1alpha2.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "0"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "0"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "384Mi", "0"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "16", "0"),
					}).
				Zone(
					topologyv1alpha2.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "0"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "0"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "512Mi", "0"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "32", "0"),
					}).Obj(),
		}
		expectedNodes := []string{"fake-node-1"}
		if len(e.expectedNodes) > 0 {
			expectedNodes = e.expectedNodes
		} else if len(e.errMsg) > 0 {
			// if there's an error we expect the pod
			// to not be found on any node
			expectedNodes = []string{}
		}
		te := nrtTestEntry{
			name:                   e.description,
			pods:                   []*v1.Pod{p.Obj()},
			nodeResourceTopologies: nodeTopologies,
			expectedNodes:          expectedNodes,
			errMsg:                 e.errMsg,
		}
		teList = append(teList, te)
	}
	return teList
}

func getPodEvents(c clientset.Interface, podNamespace, podName string) ([]v1.Event, error) {
	opts := metav1.ListOptions{
		FieldSelector: fields.SelectorFromSet(map[string]string{
			"involvedObject.name":      podName,
			"involvedObject.namespace": podNamespace,
			// TODO: use uid
		}).String(),
		TypeMeta: metav1.TypeMeta{Kind: "Pod"},
	}
	evs, err := c.CoreV1().Events(podNamespace).List(context.TODO(), opts)
	if err != nil {
		return nil, err
	}
	return evs.Items, nil
}

func filterPodFailedSchedulingEvents(events []v1.Event) []v1.Event {
	var failedSchedulingEvents []v1.Event
	for _, ev := range events {
		if ev.Reason == "FailedScheduling" {
			failedSchedulingEvents = append(failedSchedulingEvents, ev)
		}
	}
	return failedSchedulingEvents
}

func formatEvents(events []v1.Event) string {
	var sb strings.Builder
	for idx, ev := range events {
		fmt.Fprintf(&sb, "%02d - %s\n", idx, eventToString(ev))
	}
	return sb.String()
}

func eventToString(ev v1.Event) string {
	return fmt.Sprintf("type=%q action=%q message=%q reason=%q reportedBy={%s/%s}",
		ev.Type, ev.Action, ev.Message, ev.Reason, ev.ReportingController, ev.ReportingInstance,
	)
}
