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
	"testing"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	ntv1alpha1 "github.com/diktyo-io/networktopology-api/pkg/apis/networktopology/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	schedruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// Benchmark dimensions
// ---------------------------------------------------------------------------

// benchNONodeCounts are the node-count dimensions used in all NO benchmark tables.
var benchNONodeCounts = []int{10, 100, 500, 1000}

// benchNORegions / benchNOZones are the topology labels distributed across bench nodes.
var benchNORegions = []string{"us-west-1", "us-east-1"}
var benchNOZones = []string{"Z1", "Z2", "Z3", "Z4"}

// ---------------------------------------------------------------------------
// Fixture data
// ---------------------------------------------------------------------------

// benchNONetworkTopology returns a NetworkTopology CR with known, deterministic costs.
// us-west-1 → us-east-1 inter-region cost = 20
// zone Z1↔Z2: 5, Z3↔Z4: 10
func benchNONetworkTopology() *ntv1alpha1.NetworkTopology {
	return &ntv1alpha1.NetworkTopology{
		ObjectMeta: metav1.ObjectMeta{Name: "nt-test", Namespace: "default", UID: types.UID("nt-uid")},
		Spec: ntv1alpha1.NetworkTopologySpec{
			Weights: ntv1alpha1.WeightList{
				{
					Name: "UserDefined",
					TopologyList: ntv1alpha1.TopologyList{
						{
							TopologyKey: ntv1alpha1.NetworkTopologyRegion,
							OriginList: ntv1alpha1.OriginList{
								{Origin: "us-east-1", CostList: []ntv1alpha1.CostInfo{{Destination: "us-west-1", NetworkCost: 20}}},
								{Origin: "us-west-1", CostList: []ntv1alpha1.CostInfo{{Destination: "us-east-1", NetworkCost: 20}}},
							},
						},
						{
							TopologyKey: ntv1alpha1.NetworkTopologyZone,
							OriginList: ntv1alpha1.OriginList{
								{Origin: "Z1", CostList: []ntv1alpha1.CostInfo{{Destination: "Z2", NetworkCost: 5}}},
								{Origin: "Z2", CostList: []ntv1alpha1.CostInfo{{Destination: "Z1", NetworkCost: 5}}},
								{Origin: "Z3", CostList: []ntv1alpha1.CostInfo{{Destination: "Z4", NetworkCost: 10}}},
								{Origin: "Z4", CostList: []ntv1alpha1.CostInfo{{Destination: "Z3", NetworkCost: 10}}},
							},
						},
					},
				},
			},
		},
	}
}

// benchNOAppGroup returns a minimal 3-pod AppGroup: p1 depends on p2 and p3.
// MaxNetworkCost defaults to 0 → any inter-zone cost causes a violation (used in reject scenario).
func benchNOAppGroup() *agv1alpha1.AppGroup {
	return &agv1alpha1.AppGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "bench-ag", Namespace: "default", UID: types.UID("ag-uid")},
		Spec: agv1alpha1.AppGroupSpec{
			NumMembers:               3,
			TopologySortingAlgorithm: "KahnSort",
			Workloads: agv1alpha1.AppGroupWorkloadList{
				{
					Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p1-dep", Selector: "p1", APIVersion: "apps/v1", Namespace: "default"},
					Dependencies: agv1alpha1.DependenciesList{
						{Workload: agv1alpha1.AppGroupWorkloadInfo{Selector: "p2"}},
						{Workload: agv1alpha1.AppGroupWorkloadInfo{Selector: "p3"}},
					},
				},
				{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p2-dep", Selector: "p2", APIVersion: "apps/v1", Namespace: "default"}},
				{Workload: agv1alpha1.AppGroupWorkloadInfo{Kind: "Deployment", Name: "p3-dep", Selector: "p3", APIVersion: "apps/v1", Namespace: "default"}},
			},
		},
		Status: agv1alpha1.AppGroupStatus{
			RunningWorkloads: 3,
			TopologyOrder: agv1alpha1.AppGroupTopologyList{
				{Workload: agv1alpha1.AppGroupWorkloadInfo{Selector: "p1"}, Index: 1},
				{Workload: agv1alpha1.AppGroupWorkloadInfo{Selector: "p2"}, Index: 2},
				{Workload: agv1alpha1.AppGroupWorkloadInfo{Selector: "p3"}, Index: 3},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Plugin builder helper
// ---------------------------------------------------------------------------

// buildNOPlugin creates a NetworkOverhead plugin instance backed by a fake client.
// nodes is the full background node list; scheduledPods are already-placed pods that the
// pod lister will return (they drive the satisfied/violated computation).
// The function returns the plugin and a fresh CycleState.
func buildNOPlugin(
	b *testing.B,
	ctx context.Context,
	nodes []*v1.Node,
	scheduledPods []*v1.Pod,
	ag *agv1alpha1.AppGroup,
	nt *ntv1alpha1.NetworkTopology,
) *NetworkOverhead {
	b.Helper()

	s := clientgoscheme.Scheme
	utilruntime.Must(agv1alpha1.AddToScheme(s))
	utilruntime.Must(ntv1alpha1.AddToScheme(s))

	clientBuilder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(ag, nt).
		WithStatusSubresource(&agv1alpha1.AppGroup{}).
		WithStatusSubresource(&ntv1alpha1.NetworkTopology{})
	fakeClient := clientBuilder.Build()

	// Pre-populate the fake kube client so the informer has the pods immediately.
	runtimeObjs := make([]interface{}, 0, len(scheduledPods))
	for _, p := range scheduledPods {
		runtimeObjs = append(runtimeObjs, p)
	}
	_ = runtimeObjs // unused — build via testClientSet

	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	podInformer := informerFactory.Core().V1().Pods()
	podLister := podInformer.Lister()
	informerFactory.Start(ctx.Done())

	for _, p := range scheduledPods {
		if _, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			b.Fatalf("create pod %q: %v", p.Name, err)
		}
	}
	// Wait for the informer cache to sync so the lister has all pods.
	informerFactory.WaitForCacheSync(ctx.Done())

	snapshot := newTestSharedLister(nil, nodes)

	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
	}
	fh, err := tf.NewFramework(ctx, registeredPlugins, "default-scheduler",
		schedruntime.WithClientSet(cs),
		schedruntime.WithInformerFactory(informerFactory),
		schedruntime.WithSnapshotSharedLister(snapshot),
	)
	if err != nil {
		b.Fatalf("create framework: %v", err)
	}

	return &NetworkOverhead{
		Client:      fakeClient,
		podLister:   podLister,
		handle:      fh,
		namespaces:  []string{"default"},
		weightsName: "UserDefined",
		ntName:      "nt-test",
	}
}

// runNOPreFilter calls PreFilter and returns the populated CycleState.
func runNOPreFilter(b *testing.B, pl *NetworkOverhead, pod *v1.Pod) *framework.CycleState {
	b.Helper()
	state := framework.NewCycleState()
	_, status := pl.PreFilter(context.Background(), state, pod, nil)
	if !status.IsSuccess() {
		b.Fatalf("PreFilter returned non-success: %v", status)
	}
	return state
}

// benchNONodes returns n nodes assigned to random regions/zones (reproducible via index).
func benchNONodes(n int) []*v1.Node {
	nodes := make([]*v1.Node, n)
	regions := benchNORegions
	zones := benchNOZones
	for i := 0; i < n; i++ {
		r := regions[i%len(regions)]
		z := zones[i%len(zones)]
		nodes[i] = st.MakeNode().
			Name(fmt.Sprintf("n-%d", i)).
			Label(v1.LabelTopologyRegion, r).
			Label(v1.LabelTopologyZone, z).
			Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).
			Obj()
	}
	return nodes
}

// schedPods returns the two dependency pods for the bench AppGroup, both placed in us-west-1/Z1.
// Having them in the same zone as the "pass" bench node means Filter sees 0 violated dependencies.
func schedPods() []*v1.Pod {
	return []*v1.Pod{
		makePodAllocated("p2", "p2-dep", "sched-node", 0, "bench-ag", nil, nil),
		makePodAllocated("p3", "p3-dep", "sched-node", 0, "bench-ag", nil, nil),
	}
}

// schedNode is the node hosting all scheduled pods: us-west-1 / Z1.
func schedNode() *v1.Node {
	return st.MakeNode().
		Name("sched-node").
		Label(v1.LabelTopologyRegion, "us-west-1").
		Label(v1.LabelTopologyZone, "Z1").
		Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).
		Obj()
}

// passNode is in the same region+zone as schedNode → Filter: all deps satisfied.
func passNode() *v1.Node {
	return st.MakeNode().
		Name("pass-node").
		Label(v1.LabelTopologyRegion, "us-west-1").
		Label(v1.LabelTopologyZone, "Z1").
		Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).
		Obj()
}

// rejectNode is in a different region (us-east-1 / Z3).
// Inter-region cost is 20, MaxNetworkCost=0 → violated > satisfied → Filter rejects.
func rejectNode() *v1.Node {
	return st.MakeNode().
		Name("reject-node").
		Label(v1.LabelTopologyRegion, "us-east-1").
		Label(v1.LabelTopologyZone, "Z3").
		Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "8000m", v1.ResourceMemory: "16Gi"}).
		Obj()
}

// ---------------------------------------------------------------------------
// BenchmarkNetworkOverhead_PreFilter
// ---------------------------------------------------------------------------

// BenchmarkNetworkOverhead_PreFilter measures the cost of one full PreFilter cycle,
// which builds the nodeCostMap and statisticsMap across all nodes in the cluster.
// Complexity: O(n) in the number of nodes.
func BenchmarkNetworkOverhead_PreFilter(b *testing.B) {
	for _, n := range benchNONodeCounts {
		n := n
		b.Run(fmt.Sprintf("%dnodes", n), func(b *testing.B) {
			ctx := context.Background()
			ag := benchNOAppGroup()
			nt := benchNONetworkTopology()
			sp := schedPods()

			// Background nodes + the node hosting scheduled pods.
			nodes := append(benchNONodes(n), schedNode())

			pl := buildNOPlugin(b, ctx, nodes, sp, ag, nt)
			pod := makePod("p1", "p1-dep", 0, "bench-ag", nil, nil)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				state := framework.NewCycleState()
				pl.PreFilter(ctx, state, pod, nil) //nolint:errcheck
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BenchmarkNetworkOverhead_Filter
// ---------------------------------------------------------------------------

// BenchmarkNetworkOverhead_Filter measures the per-node Filter cost after PreFilter has
// already populated the CycleState. Filter is O(1) — a map lookup plus optional status
// allocation when the node is rejected.
func BenchmarkNetworkOverhead_Filter(b *testing.B) {
	for _, n := range benchNONodeCounts {
		n := n
		for _, scenario := range []string{"pass", "reject"} {
			scenario := scenario
			b.Run(fmt.Sprintf("%dnodes/%s", n, scenario), func(b *testing.B) {
				ctx := context.Background()
				ag := benchNOAppGroup()
				nt := benchNONetworkTopology()
				sp := schedPods()

				// Choose the target node for this scenario.
				var target *v1.Node
				if scenario == "pass" {
					target = passNode()
				} else {
					target = rejectNode()
				}

				nodes := append(benchNONodes(n), schedNode(), passNode(), rejectNode())
				pl := buildNOPlugin(b, ctx, nodes, sp, ag, nt)
				pod := makePod("p1", "p1-dep", 0, "bench-ag", nil, nil)

				// Build CycleState once (not measured).
				state := runNOPreFilter(b, pl, pod)

				nodeInfo := framework.NewNodeInfo()
				nodeInfo.SetNode(target)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					pl.Filter(ctx, state, pod, nodeInfo) //nolint:errcheck
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// BenchmarkNetworkOverhead_Score
// ---------------------------------------------------------------------------

// BenchmarkNetworkOverhead_Score measures the per-node Score cost after PreFilter.
// "with_cost" reads an accumulated cost from finalCostMap (O(1) map lookup).
// "score_equally" uses the fast path that returns the minimum score immediately.
func BenchmarkNetworkOverhead_Score(b *testing.B) {
	for _, n := range benchNONodeCounts {
		n := n
		for _, scenario := range []string{"with_cost", "score_equally"} {
			scenario := scenario
			b.Run(fmt.Sprintf("%dnodes/%s", n, scenario), func(b *testing.B) {
				ctx := context.Background()
				ag := benchNOAppGroup()
				nt := benchNONetworkTopology()

				var sp []*v1.Pod
				var pod *v1.Pod

				if scenario == "with_cost" {
					// Scheduled pods present → full scoring path.
					sp = schedPods()
					nodes := append(benchNONodes(n), schedNode(), passNode(), rejectNode())
					pl := buildNOPlugin(b, ctx, nodes, sp, ag, nt)
					pod = makePod("p1", "p1-dep", 0, "bench-ag", nil, nil)
					state := runNOPreFilter(b, pl, pod)
					nodeInfo := framework.NewNodeInfo()
					nodeInfo.SetNode(rejectNode()) // will be in finalCostMap

					b.ResetTimer()
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						pl.Score(ctx, state, pod, nodeInfo) //nolint:errcheck
					}
					return
				}

				// score_equally: no scheduled pods → scoreEqually=true → returns minScore.
				sp = nil
				nodes := benchNONodes(n)
				pl := buildNOPlugin(b, ctx, nodes, sp, ag, nt)
				pod = makePod("p1", "p1-dep", 0, "bench-ag", nil, nil)
				state := runNOPreFilter(b, pl, pod)
				nodeInfo := framework.NewNodeInfo()
				nodeInfo.SetNode(nodes[0])

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					pl.Score(ctx, state, pod, nodeInfo) //nolint:errcheck
				}
			})
		}
	}
}
