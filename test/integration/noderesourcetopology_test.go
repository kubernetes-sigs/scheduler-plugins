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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/test/util"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
)

const (
	cpu             = string(v1.ResourceCPU)
	memory          = string(v1.ResourceMemory)
	hugepages2Mi    = "hugepages-2Mi"
	nicResourceName = "vendor/nic1"
)

const (
	testPodName = "topology-aware-scheduler-pod"
)

var (
	mostAllocatedScheduler      = fmt.Sprintf("%v-scheduler", string(scheconfig.MostAllocated))
	balancedAllocationScheduler = fmt.Sprintf("%v-scheduler", string(scheconfig.BalancedAllocation))
	leastAllocatedScheduler     = fmt.Sprintf("%v-scheduler", string(scheconfig.LeastAllocated))
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
}

type nrtTestEntry struct {
	name                   string
	pods                   []*v1.Pod
	nodeResourceTopologies []*topologyv1alpha1.NodeResourceTopology
	expectedNodes          []string
	errMsg                 string
}

func TestTopologyMatchPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := versioned.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
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
		v1.ResourceCPU:    "64",
		v1.ResourceMemory: "128Gi",
		v1.ResourcePods:   "32",
		hugepages2Mi:      "896Mi",
		nicResourceName:   "48",
	}
	for _, nodeName := range []string{"fake-node-1", "fake-node-2"} {
		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).Capacity(resList).Obj()
		n, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}

		t.Logf(" Node %s created: %v", nodeName, n)
	}

	nodeList, err := cs.CoreV1().Nodes().List(testCtx.Ctx, metav1.ListOptions{})
	t.Logf("NodeList: %v", nodeList)
	pause := imageutils.GetPauseImageName()
	tests := []nrtTestEntry{
		{
			name: "Filtering out nodes that cannot fit resources on a single numa node in case of Guaranteed pod",
			pods: []*v1.Pod{
				withLimits(st.MakePod().Namespace(ns).Name(testPodName), map[string]string{cpu: "4", memory: "5Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
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
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "0", "0"),
						}).Obj(),

				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
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
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy("foo").
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo("foo", "2", "2"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with most-allocated strategy scheduler",
			pods: []*v1.Pod{
				withLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(mostAllocatedScheduler),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				withLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(balancedAllocationScheduler),
					map[string]string{cpu: "2", memory: "2Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with least-allocated strategy scheduler",
			pods: []*v1.Pod{
				withLimits(st.MakePod().Namespace(ns).Name(testPodName).SchedulerName(leastAllocatedScheduler),
					map[string]string{cpu: "1", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
							noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
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
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodePodLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodePodLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
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
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "3", memory: "5Gi"}, false),
					map[string]string{cpu: "3", memory: "5Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "8", "6"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "SingleNUMANodeContainerLevel: Filtering out nodes that cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "4Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "4", memory: "4Gi"}, false),
					map[string]string{cpu: "2", memory: "4Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "6", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "3"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{"fake-node-1"},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "10Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with init container",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "2", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "10Gi"}, true).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
		},
		{
			name: "Negative: SingleNUMANodeContainerLevel: Cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name(testPodName),
						map[string]string{cpu: "4", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "6Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).Obj(),
				MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						}).
					Zone(
						topologyv1alpha1.ResourceInfoList{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						}).Obj(),
			},
			expectedNodes: []string{},
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
			errMsg: "cannot align container: cnt-2",
		},
		{
			description: "[5][tier1] multi containers with device over allocation, spread across NUMAs - not fit",
			cntReq: []map[string]string{
				{cpu: "2", memory: "6Gi", hugepages2Mi: "50Mi", nicResourceName: "20"},
				{cpu: "2", memory: "6Gi", hugepages2Mi: "500Mi", nicResourceName: "20"},
			},
			errMsg: "cannot align container: cnt-2",
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
			errMsg: "cannot align init container: initcnt-1",
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
			errMsg: "cannot align init container: initcnt-1",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-3",
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
			errMsg: "cannot align container: cnt-1",
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
			errMsg: "cannot align init container: initcnt-1",
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
			errMsg: "cannot align init container: initcnt-2",
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
						t.Logf("Pod %q is on a nodes as expected.", p.Name)
					} else {
						t.Errorf("Pod %s is expected on node %s, but found on node %s",
							p.Name, tt.expectedNodes, nodeName)
					}
					// if tt.expectedNodes == 0 we don't expect the pod to get scheduled
				} else {
					// wait for the pod scheduling to failed.
					if err := wait.Poll(5*time.Second, 20*time.Second, func() (bool, error) {
						events, err := podFailedScheduling(cs, ns, p.Name)
						if err != nil {
							// This could be a connection error, so we want to retry.
							klog.ErrorS(err, "Failed check pod scheduling status for pod", "pod", klog.KRef(ns, p.Name))
							return false, nil
						}
						for _, e := range events {
							if strings.Contains(e.Message, tt.errMsg) {
								return true, nil
							}
							klog.Warningf("Pod failed but error message does not contain substring: %q; got %q instead", tt.errMsg, e.Message)
						}
						return false, nil
					}); err != nil {
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

// getNodeName returns the name of the node if a node has assigned to the given pod
func getNodeName(ctx context.Context, c clientset.Interface, podNamespace, podName string) (string, error) {
	pod, err := c.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Spec.NodeName, nil
}

func createNodeResourceTopologies(ctx context.Context, topologyClient *versioned.Clientset, noderesourcetopologies []*topologyv1alpha1.NodeResourceTopology) error {
	for _, nrt := range noderesourcetopologies {
		_, err := topologyClient.TopologyV1alpha1().NodeResourceTopologies().Create(ctx, nrt, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupNodeResourceTopologies(ctx context.Context, topologyClient *versioned.Clientset, noderesourcetopologies []*topologyv1alpha1.NodeResourceTopology) {
	for _, nrt := range noderesourcetopologies {
		err := topologyClient.TopologyV1alpha1().NodeResourceTopologies().Delete(ctx, nrt.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to clean up NodeResourceTopology", "nodeResourceTopology", nrt)
		}
	}
}

// withLimits adds a new app or init container to the inner pod with a given resource map.
func withLimits(p *st.PodWrapper, resMap map[string]string, initContainer bool) *st.PodWrapper {
	if len(resMap) == 0 {
		return p
	}

	res := v1.ResourceList{}
	for k, v := range resMap {
		res[v1.ResourceName(k)] = resource.MustParse(v)
	}

	var containers *[]v1.Container
	var cntName string
	if initContainer {
		containers = &p.Obj().Spec.InitContainers
		cntName = "initcnt"
	} else {
		containers = &p.Obj().Spec.Containers
		cntName = "cnt"
	}

	*containers = append(*containers, v1.Container{
		Name:  fmt.Sprintf("%s-%d", cntName, len(*containers)+1),
		Image: imageutils.GetPauseImageName(),
		Resources: v1.ResourceRequirements{
			Limits: res,
		},
	})

	return p
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

func makeResourceAllocationScoreArgs(strategy *scheconfig.ScoringStrategy) *scheconfig.NodeResourceTopologyMatchArgs {
	return &scheconfig.NodeResourceTopologyMatchArgs{
		ScoringStrategy: *strategy,
	}
}

type nrtWrapper struct {
	nrt topologyv1alpha1.NodeResourceTopology
}

func MakeNRT() *nrtWrapper {
	return &nrtWrapper{topologyv1alpha1.NodeResourceTopology{}}
}

func (n *nrtWrapper) Name(name string) *nrtWrapper {
	n.nrt.Name = name
	return n
}

func (n *nrtWrapper) Policy(policy topologyv1alpha1.TopologyManagerPolicy) *nrtWrapper {
	n.nrt.TopologyPolicies = append(n.nrt.TopologyPolicies, string(policy))
	return n
}

func (n *nrtWrapper) Zone(resInfo topologyv1alpha1.ResourceInfoList) *nrtWrapper {
	z := topologyv1alpha1.Zone{
		Name:      fmt.Sprintf("node-%d", len(n.nrt.Zones)),
		Type:      "Node",
		Resources: resInfo,
	}
	n.nrt.Zones = append(n.nrt.Zones, z)
	return n
}

func (n *nrtWrapper) Obj() *topologyv1alpha1.NodeResourceTopology {
	return &n.nrt
}

func parseTestUserEntry(entries []nrtTestUserEntry, ns string) []nrtTestEntry {
	var teList []nrtTestEntry
	for i, e := range entries {
		p := st.MakePod().Name(fmt.Sprintf("%s-%d", testPodName, i+1)).Namespace(ns)
		for _, req := range e.initCntReq {
			p = withLimits(p, req, true)
		}

		for _, req := range e.cntReq {
			p = withLimits(p, req, false)
		}
		nodeTopologies := []*topologyv1alpha1.NodeResourceTopology{
			MakeNRT().Name("fake-node-1").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
				Zone(
					topologyv1alpha1.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "30"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "60Gi"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "384Mi", "384Mi"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "16", "16")}).
				Zone(
					topologyv1alpha1.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "32"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "64Gi"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "512Mi", "512Mi"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "32", "32"),
					}).Obj(),
			// we set all available resource as 0
			// because we want all these tests to be running against fake-node-1
			MakeNRT().Name("fake-node-2").Policy(topologyv1alpha1.SingleNUMANodeContainerLevel).
				Zone(
					topologyv1alpha1.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "0"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "0"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "384Mi", "0"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "16", "0"),
					}).
				Zone(
					topologyv1alpha1.ResourceInfoList{
						noderesourcetopology.MakeTopologyResInfo(cpu, "32", "0"),
						noderesourcetopology.MakeTopologyResInfo(memory, "64Gi", "0"),
						noderesourcetopology.MakeTopologyResInfo(hugepages2Mi, "512Mi", "0"),
						noderesourcetopology.MakeTopologyResInfo(nicResourceName, "32", "0"),
					}).Obj(),
		}
		expectedNodes := []string{"fake-node-1"}
		// if there's an error we expect the pod
		// to not be found on any node
		if len(e.errMsg) > 0 {
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

func podFailedScheduling(c clientset.Interface, podNamespace, podName string) ([]v1.Event, error) {
	var failedSchedulingEvents []v1.Event
	opt := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		TypeMeta:      metav1.TypeMeta{Kind: "Pod"},
	}
	events, err := c.CoreV1().Events(podNamespace).List(context.TODO(), opt)
	if err != nil {
		return failedSchedulingEvents, err
	}

	for _, e := range events.Items {
		if e.Reason == "FailedScheduling" {
			failedSchedulingEvents = append(failedSchedulingEvents, e)
		}
	}
	return failedSchedulingEvents, nil
}
