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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	cpu    = string(v1.ResourceCPU)
	memory = string(v1.ResourceMemory)
)

var (
	mostAllocatedScheduler      = fmt.Sprintf("%v-scheduler", string(scheconfig.MostAllocated))
	balancedAllocationScheduler = fmt.Sprintf("%v-scheduler", string(scheconfig.BalancedAllocation))
	leastAllocatedScheduler     = fmt.Sprintf("%v-scheduler", string(scheconfig.LeastAllocated))
)

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
		v1.ResourceCPU:    "16",
		v1.ResourceMemory: "100Gi",
		v1.ResourcePods:   "32",
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
				withLimits(st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"), map[string]string{cpu: "4", memory: "5Gi"}, false).Obj(),
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
				st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(),
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
				st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").Req(map[v1.ResourceName]string{v1.ResourceMemory: "5Gi"}).Obj(),
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
				withLimits(st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(mostAllocatedScheduler),
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
				withLimits(st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(balancedAllocationScheduler),
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
				withLimits(st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(leastAllocatedScheduler),
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
				st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(mostAllocatedScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(balancedAllocationScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with least-allocated strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod").SchedulerName(leastAllocatedScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "SingleNUMANodePodLevel: Filtering out nodes that cannot fit resources in case of Guaranteed pod with multi containers",
			pods: []*v1.Pod{
				withLimits(
					withLimits(
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
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
						st.MakePod().Namespace(ns).Name("topology-aware-scheduler-pod"),
						map[string]string{cpu: "4", memory: "4Gi"}, false),
					map[string]string{cpu: "4", memory: "6Gi"}, false).Obj(),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				makeNodeResourceTopology("fake-node-1", topologyv1alpha1.SingleNUMANodeContainerLevel,
					[]topologyv1alpha1.ResourceInfoList{
						{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						},
						{
							noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						},
					}),
				makeNodeResourceTopology("fake-node-2", topologyv1alpha1.SingleNUMANodeContainerLevel,
					[]topologyv1alpha1.ResourceInfoList{
						{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
						},
						{
							noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
							noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "3Gi"),
						},
					}),
			},
			expectedNodes: []string{},
		},
	} {
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
					if err := wait.Poll(1*time.Second, 20*time.Second, func() (bool, error) {
						return !podScheduled(cs, ns, p.Name), nil
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
		if err != nil && !errors.IsAlreadyExists(err) {
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
		cntName = "initcon"
	} else {
		containers = &p.Obj().Spec.Containers
		cntName = "con"
	}

	*containers = append(*containers, v1.Container{
		Name:  fmt.Sprintf("%s-%d", cntName, len(*containers)),
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

func makeNodeResourceTopology(name string, tmp topologyv1alpha1.TopologyManagerPolicy, resInfoLists []topologyv1alpha1.ResourceInfoList) *topologyv1alpha1.NodeResourceTopology {
	nrt := &topologyv1alpha1.NodeResourceTopology{
		ObjectMeta:       metav1.ObjectMeta{Name: name},
		TopologyPolicies: []string{string(tmp)},
		Zones:            topologyv1alpha1.ZoneList{},
	}

	for i, resInfoList := range resInfoLists {
		z := topologyv1alpha1.Zone{
			Name:      fmt.Sprintf("node-%d", i),
			Type:      "Node",
			Resources: resInfoList,
		}
		nrt.Zones = append(nrt.Zones, z)
	}
	return nrt
}
