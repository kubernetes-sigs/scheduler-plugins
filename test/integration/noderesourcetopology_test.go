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
	"io/ioutil"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testfwk "k8s.io/kubernetes/test/integration/framework"
	testutil "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"
	scheconfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/test/util"
	"sigs.k8s.io/yaml"

	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned"
)

const (
	cpu    = string(v1.ResourceCPU)
	memory = string(v1.ResourceMemory)
)

var (
	leastAllocatableScheduler   = fmt.Sprintf("%v-scheduler", string(scheconfig.MostAllocated))
	balancedAllocationScheduler = fmt.Sprintf("%v-scheduler", string(scheconfig.BalancedAllocation))
	mostAllocatableScheduler    = fmt.Sprintf("%v-scheduler", string(scheconfig.LeastAllocated))
)

func TestTopologyMatchPlugin(t *testing.T) {
	t.Log("Creating API Server...")
	// Start API Server with apiextensions supported.
	server := apiservertesting.StartTestServerOrDie(
		t, apiservertesting.NewDefaultTestServerOptions(),
		[]string{"--disable-admission-plugins=ServiceAccount,TaintNodesByCondition,Priority", "--runtime-config=api/all=true"},
		testfwk.SharedEtcd(),
	)
	testCtx := &testutil.TestContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())
	testCtx.CloseFn = func() { server.TearDownFn() }

	t.Log("Creating CRD...")
	apiExtensionClient := apiextensionsclient.NewForConfigOrDie(server.ClientConfig)
	ctx := testCtx.Ctx
	if _, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Create(testCtx.Ctx, makeNodeResourceTopologyCRD(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	server.ClientConfig.ContentType = "application/json"
	testCtx.KubeConfig = server.ClientConfig
	cs := kubernetes.NewForConfigOrDie(testCtx.KubeConfig)
	testCtx.ClientSet = cs
	extClient := versioned.NewForConfigOrDie(testCtx.KubeConfig)

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

	ns, err := cs.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))}}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("Failed to create integration test ns: %v", err)
	}

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
			leastAllocatableScheduler,
			makeResourceAllocationScoreArgs(ns.Name, &scheconfig.ScoringStrategy{Type: scheconfig.MostAllocated}),
		),
		// a profile with both the filter and score enabled and score strategy is BalancedAllocation
		makeProfileByPluginArgs(
			balancedAllocationScheduler,
			makeResourceAllocationScoreArgs(ns.Name, &scheconfig.ScoringStrategy{Type: scheconfig.BalancedAllocation}),
		),
		// a profile with both the filter and score enabled and score strategy is LeastAllocated
		makeProfileByPluginArgs(
			mostAllocatableScheduler,
			makeResourceAllocationScoreArgs(ns.Name, &scheconfig.ScoringStrategy{Type: scheconfig.LeastAllocated}),
		),
	)

	testCtx = util.InitTestSchedulerWithOptions(
		t,
		testCtx,
		true,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{noderesourcetopology.Name: noderesourcetopology.New}),
	)
	t.Log("Init scheduler success")
	defer testutil.CleanupTest(t, testCtx)

	// Create a Node.
	resList := map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "100Gi",
		v1.ResourcePods:   "32",
	}
	for _, nodeName := range []string{"fake-node-1", "fake-node-2"} {
		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).Capacity(resList).Obj()
		n, err := cs.CoreV1().Nodes().Create(ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}

		t.Logf(" Node %s created: %v", nodeName, n)
	}

	nodeList, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	t.Logf("NodeList: %v", nodeList)
	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name                   string
		pods                   []*v1.Pod
		nodeResourceTopologies []*topologyv1alpha1.NodeResourceTopology
		expectedNodes          []string
	}{
		{
			name: "Filtering out nodes that cannot fit resources on a single numa node in case of Guaranteed pod",
			pods: []*v1.Pod{
				withContainer(withReqAndLimit(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod"), map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "5Gi"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
								topologyv1alpha1.ResourceInfo{
									Name:      memory,
									Available: resource.MustParse("8Gi"),
									Capacity:  resource.MustParse("8Gi"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
								topologyv1alpha1.ResourceInfo{
									Name:      memory,
									Available: resource.MustParse("8Gi"),
									Capacity:  resource.MustParse("8Gi"),
								},
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("4"),
									Capacity:  resource.MustParse("4"),
								},
								topologyv1alpha1.ResourceInfo{
									Name:      memory,
									Available: resource.MustParse("8Gi"),
									Capacity:  resource.MustParse("8Gi"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("0"),
									Capacity:  resource.MustParse("0"),
								},
								topologyv1alpha1.ResourceInfo{
									Name:      memory,
									Available: resource.MustParse("8Gi"),
									Capacity:  resource.MustParse("8Gi"),
								},
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling of a burstable pod requesting only cpus",
			pods: []*v1.Pod{
				withContainer(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("4"),
									Capacity:  resource.MustParse("4"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("0"),
									Capacity:  resource.MustParse("0"),
								},
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      cpu,
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling of a burstable pod requesting only memory",
			pods: []*v1.Pod{
				withContainer(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").Req(map[v1.ResourceName]string{v1.ResourceMemory: "5Gi"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      "foo",
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      "foo",
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{"foo"},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      "foo",
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								topologyv1alpha1.ResourceInfo{
									Name:      "foo",
									Available: resource.MustParse("2"),
									Capacity:  resource.MustParse("2"),
								},
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with least-allocatable strategy scheduler",
			pods: []*v1.Pod{
				withContainer(withReqAndLimit(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(leastAllocatableScheduler),
					map[v1.ResourceName]string{v1.ResourceCPU: "1", v1.ResourceMemory: "4Gi"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
								noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
								noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
								noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
								noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				withContainer(withReqAndLimit(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(balancedAllocationScheduler),
					map[v1.ResourceName]string{v1.ResourceCPU: "2", v1.ResourceMemory: "2Gi"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
								noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "4", "4"),
								noderesourcetopology.MakeTopologyResInfo(memory, "50Gi", "50Gi"),
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
								noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "6", "6"),
								noderesourcetopology.MakeTopologyResInfo(memory, "6Gi", "6Gi"),
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Scheduling Guaranteed pod with most-allocatable strategy scheduler",
			pods: []*v1.Pod{
				withContainer(withReqAndLimit(st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(mostAllocatableScheduler),
					map[v1.ResourceName]string{v1.ResourceCPU: "1", v1.ResourceMemory: "4Gi"}).Obj(), pause),
			},
			nodeResourceTopologies: []*topologyv1alpha1.NodeResourceTopology{
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-1"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
								noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "2", "2"),
								noderesourcetopology.MakeTopologyResInfo(memory, "8Gi", "8Gi"),
							},
						},
					},
				},
				{
					ObjectMeta:       metav1.ObjectMeta{Name: "fake-node-2"},
					TopologyPolicies: []string{string(topologyv1alpha1.SingleNUMANodeContainerLevel)},
					Zones: topologyv1alpha1.ZoneList{
						topologyv1alpha1.Zone{
							Name: "node-0",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
								noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
							},
						},
						topologyv1alpha1.Zone{
							Name: "node-1",
							Type: "Node",
							Resources: topologyv1alpha1.ResourceInfoList{
								noderesourcetopology.MakeTopologyResInfo(cpu, "1", "1"),
								noderesourcetopology.MakeTopologyResInfo(memory, "4Gi", "4Gi"),
							},
						},
					},
				},
			},
			expectedNodes: []string{"fake-node-1"},
		},
		// in the following "Scheduling Best-Effort pod with least-allocatable/balanced-allocation/most-allocatable strategy" tests,
		// both nodes are expected to get the maximum score.
		// in case of multiple nodes with the same highest score, the chosen node selection might vary.
		// thus, we can't tell exactly which node will get selected, so the only option is to accept both of them,
		// and it's still good enough for making sure that pod not stuck in pending state and/or the program isn't crashing
		{
			name: "Scheduling Best-Effort pod with least-allocatable strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(leastAllocatableScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with balanced-allocation strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(balancedAllocationScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Scheduling Best-Effort pod with most-allocatable strategy scheduler",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns.Name).Name("topology-aware-scheduler-pod").SchedulerName(mostAllocatableScheduler).Container(pause).Obj(),
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-topology-match-test %v", tt.name)
			defer cleanupNodeResourceTopologies(ctx, extClient, tt.nodeResourceTopologies)
			defer testutil.CleanupPods(cs, t, tt.pods)

			if err := createNodeResourceTopologies(ctx, extClient, tt.nodeResourceTopologies); err != nil {
				t.Fatal(err)
			}

			// Create Pods
			for _, p := range tt.pods {
				t.Logf("Creating Pod %q", p.Name)
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), p, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", p.Name, err)
				}
			}

			for _, p := range tt.pods {
				// Wait for the pod to be scheduled.
				if err := wait.Poll(1*time.Second, 20*time.Second, func() (bool, error) {
					return podScheduled(cs, ns.Name, p.Name), nil
				}); err != nil {
					t.Errorf("pod %q to be scheduled, error: %v", p.Name, err)
				}

				t.Logf("Pod %v scheduled", p.Name)

				// The other pods should be scheduled on the small nodes.
				nodeName, err := getNodeName(cs, ns.Name, p.Name)
				if err != nil {
					t.Log(err)
				}
				if contains(tt.expectedNodes, nodeName) {
					t.Logf("Pod %q is on a nodes as expected.", p.Name)
				} else {
					t.Errorf("Pod %s is expected on node %s, but found on node %s",
						p.Name, tt.expectedNodes, nodeName)
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
func getNodeName(c clientset.Interface, podNamespace, podName string) (string, error) {
	pod, err := c.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Spec.NodeName, nil
}

// makeNodeResourceTopologyCRD prepares a CRD.
func makeNodeResourceTopologyCRD() *apiextensionsv1.CustomResourceDefinition {
	content, err := ioutil.ReadFile("../../manifests/noderesourcetopology/crd.yaml")
	if err != nil {
		return &apiextensionsv1.CustomResourceDefinition{}
	}

	noderesourcetopologyCRD := &apiextensionsv1.CustomResourceDefinition{}
	err = yaml.Unmarshal(content, noderesourcetopologyCRD)
	if err != nil {
		return &apiextensionsv1.CustomResourceDefinition{}
	}

	return noderesourcetopologyCRD
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

func withContainer(pod *v1.Pod, image string) *v1.Pod {
	pod.Spec.Containers[0].Name = "con0"
	pod.Spec.Containers[0].Image = image
	return pod
}

// withReqAndLimit adds a new container to the inner pod with given resource map.
func withReqAndLimit(p *st.PodWrapper, resMap map[v1.ResourceName]string) *st.PodWrapper {
	if len(resMap) == 0 {
		return p
	}
	res := v1.ResourceList{}
	for k, v := range resMap {
		res[k] = resource.MustParse(v)
	}
	p.Spec.Containers = append(p.Spec.Containers, v1.Container{
		Resources: v1.ResourceRequirements{
			Requests: res,
			Limits:   res,
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

func makeResourceAllocationScoreArgs(ns string, strategy *scheconfig.ScoringStrategy) *scheconfig.NodeResourceTopologyMatchArgs {
	return &scheconfig.NodeResourceTopologyMatchArgs{
		ScoringStrategy: *strategy,
	}
}
