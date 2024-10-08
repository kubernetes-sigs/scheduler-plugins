/*
Copyright 2020 The Kubernetes Authors.

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

package capacityscheduling

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	gocmp "github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

const ResourceGPU v1.ResourceName = "nvidia.com/gpu"

var (
	midPriority, highPriority = int32(100), int32(1000)
)

func TestPreFilter(t *testing.T) {
	type podInfo struct {
		podName      string
		podNamespace string
		memReq       int64
	}

	tests := []struct {
		name          string
		podInfos      []podInfo
		elasticQuotas map[string]*ElasticQuotaInfo
		expected      []framework.Code
	}{
		{
			name: "pod subjects to ElasticQuota",
			podInfos: []podInfo{
				{podName: "ns1-p1", podNamespace: "ns1", memReq: 500},
				{podName: "ns1-p2", podNamespace: "ns1", memReq: 1800},
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 300,
					},
				},
			},
			expected: []framework.Code{
				framework.Success,
				framework.Unschedulable,
			},
		},
		{
			name: "the sum of used is bigger than the sum of min",
			podInfos: []podInfo{
				{podName: "ns2-p1", podNamespace: "ns2", memReq: 500},
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 1800,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 200,
					},
				},
			},
			expected: []framework.Code{
				framework.Unschedulable,
			},
		},
		{
			name: "without elasticQuotaInfo",
			podInfos: []podInfo{
				{podName: "ns2-p1", podNamespace: "ns2", memReq: 500},
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{},
			expected: []framework.Code{
				framework.Success,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: tt.elasticQuotas,
				fh:                fwk,
			}

			pods := make([]*v1.Pod, 0)
			for _, podInfo := range tt.podInfos {
				pod := makePod(podInfo.podName, podInfo.podNamespace, podInfo.memReq, 0, 0, 0, podInfo.podName, "")
				pods = append(pods, pod)
			}

			state := framework.NewCycleState()
			for i := range pods {
				if _, got := cs.PreFilter(context.TODO(), state, pods[i]); got.Code() != tt.expected[i] {
					t.Errorf("expected %v, got %v : %v", tt.expected[i], got.Code(), got.Message())
				}
			}
		})
	}
}

func TestPostFilter(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150"}
	tests := []struct {
		name                  string
		pod                   *v1.Pod
		existPods             []*v1.Pod
		nodes                 []*v1.Node
		filteredNodesStatuses framework.NodeToStatusMap
		elasticQuotas         map[string]*ElasticQuotaInfo
		wantResult            *framework.PostFilterResult
		wantStatus            *framework.Status
	}{
		{
			name: "in-namespace preemption",
			pod:  makePod("t1-p1", "ns1", 50, 0, 0, highPriority, "t1-p1", ""),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
				makePod("t1-p4", "ns2", 50, 0, 0, midPriority, "t1-p4", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 200,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node-a"),
			wantStatus: framework.NewStatus(framework.Success),
		},
		{
			name: "cross-namespace preemption",
			pod:  makePod("t1-p1", "ns1", 50, 0, 0, highPriority, "t1-p1", ""),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
				makePod("t1-p4", "ns2", 50, 0, 0, midPriority, "t1-p4", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 150,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node-a"),
			wantStatus: framework.NewStatus(framework.Success),
		},
		{
			name: "without elasticQuotas",
			pod:  makePod("t1-p1", "ns1", 50, 0, 0, highPriority, "t1-p1", ""),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
				makePod("t1-p4", "ns2", 50, 0, 0, midPriority, "t1-p4", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{},
			wantResult:    framework.NewPostFilterResultWithNominatedNode("node-a"),
			wantStatus:    framework.NewStatus(framework.Success),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := makeRegisteredPlugin()

			podItems := []v1.Pod{}
			for _, pod := range tt.existPods {
				podItems = append(podItems, *pod)
			}
			cs := clientsetfake.NewSimpleClientset(&v1.PodList{Items: podItems})
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			podInformer.GetStore().Add(tt.pod)
			for i := range tt.existPods {
				podInformer.GetStore().Add(tt.existPods[i])
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fwk, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.existPods, tt.nodes)),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			_, preFilterStatus := fwk.RunPreFilterPlugins(ctx, state, tt.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}

			podReq := computePodResourceRequest(tt.pod)
			elasticQuotaSnapshotState := &ElasticQuotaSnapshotState{
				elasticQuotaInfos: tt.elasticQuotas,
			}
			prefilterState := &PreFilterState{
				podReq:                         *podReq,
				nominatedPodsReqWithPodReq:     *podReq,
				nominatedPodsReqInEQWithPodReq: *podReq,
			}
			state.Write(preFilterStateKey, prefilterState)
			state.Write(ElasticQuotaSnapshotKey, elasticQuotaSnapshotState)

			c := &CapacityScheduling{
				elasticQuotaInfos: tt.elasticQuotas,
				fh:                fwk,
				podLister:         informerFactory.Core().V1().Pods().Lister(),
				pdbLister:         getPDBLister(informerFactory),
			}
			gotResult, gotStatus := c.PostFilter(ctx, state, tt.pod, tt.filteredNodesStatuses)
			if diff := gocmp.Diff(tt.wantStatus, gotStatus); diff != "" {
				t.Errorf("Unexpected status (-want, +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantResult, gotResult); diff != "" {
				t.Errorf("Unexpected postFilterResult (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestReserve(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*v1.Pod
		elasticQuotas map[string]*ElasticQuotaInfo
		expectedCodes []framework.Code
		expected      []map[string]*ElasticQuotaInfo
	}{
		{
			name: "Reserve pods",
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 50, 0, 0, midPriority, "t1-p2", "node-a"),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 300,
					},
				},
			},
			expectedCodes: []framework.Code{
				framework.Success,
				framework.Success,
			},
			expected: []map[string]*ElasticQuotaInfo{
				{
					"ns1": {
						Namespace: "ns1",
						pods:      sets.NewString("t1-p1"),
						Min: &framework.Resource{
							Memory: 1000,
						},
						Max: &framework.Resource{
							Memory: 2000,
						},
						Used: &framework.Resource{
							Memory: 350,
							ScalarResources: map[v1.ResourceName]int64{
								ResourceGPU: 0,
							},
						},
					},
				},
				{
					"ns1": {
						Namespace: "ns1",
						pods:      sets.NewString("t1-p1"),
						Min: &framework.Resource{
							Memory: 1000,
						},
						Max: &framework.Resource{
							Memory: 2000,
						},
						Used: &framework.Resource{
							Memory: 350,
							ScalarResources: map[v1.ResourceName]int64{
								ResourceGPU: 0,
							},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: tt.elasticQuotas,
				fh:                fwk,
			}

			state := framework.NewCycleState()
			for i, pod := range tt.pods {
				got := cs.Reserve(nil, state, pod, "node-a")
				if got.Code() != tt.expectedCodes[i] {
					t.Errorf("expected %v, got %v : %v", tt.expected[i], got.Code(), got.Message())
				}
				if !reflect.DeepEqual(cs.elasticQuotaInfos["ns1"], tt.expected[i]["ns1"]) {
					t.Errorf("expected %v, got %v", tt.expected[i]["ns1"], cs.elasticQuotaInfos["ns1"])
				}
			}
		})
	}
}

func TestUnreserve(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*v1.Pod
		elasticQuotas map[string]*ElasticQuotaInfo
		expected      []map[string]*ElasticQuotaInfo
	}{
		{
			name: "Unreserve pods",
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns1", 50, 0, 0, midPriority, "t1-p3", "node-a"),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.NewString("t1-p3", "t1-p4"),
					Min: &framework.Resource{
						Memory: 1000,
					},
					Max: &framework.Resource{
						Memory: 2000,
					},
					Used: &framework.Resource{
						Memory: 300,
					},
				},
			},
			expected: []map[string]*ElasticQuotaInfo{
				{
					"ns1": {
						Namespace: "ns1",
						pods:      sets.NewString("t1-p3", "t1-p4"),
						Min: &framework.Resource{
							Memory: 1000,
						},
						Max: &framework.Resource{
							Memory: 2000,
						},
						Used: &framework.Resource{
							Memory: 300,
						},
					},
				},
				{
					"ns1": {
						Namespace: "ns1",
						pods:      sets.NewString("t1-p3", "t1-p4"),
						Min: &framework.Resource{
							Memory: 1000,
						},
						Max: &framework.Resource{
							Memory: 2000,
						},
						Used: &framework.Resource{
							Memory: 300,
						},
					},
				},
				{
					"ns1": {
						Namespace: "ns1",
						pods:      sets.NewString("t1-p4"),
						Min: &framework.Resource{
							Memory: 1000,
						},
						Max: &framework.Resource{
							Memory: 2000,
						},
						Used: &framework.Resource{
							Memory: 250,
							ScalarResources: map[v1.ResourceName]int64{
								ResourceGPU: 0,
							},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: tt.elasticQuotas,
				fh:                fwk,
			}

			state := framework.NewCycleState()
			for i, pod := range tt.pods {
				cs.Unreserve(nil, state, pod, "node-a")
				if !reflect.DeepEqual(cs.elasticQuotaInfos["ns1"], tt.expected[i]["ns1"]) {
					t.Errorf("expected %#v, got %#v", tt.expected[i]["ns1"].Used, cs.elasticQuotaInfos["ns1"].Used)
				}
			}
		})
	}
}

func TestDryRunPreemption(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150"}
	tests := []struct {
		name          string
		pod           *v1.Pod
		pods          []*v1.Pod
		nodes         []*v1.Node
		nodesStatuses framework.NodeToStatusMap
		elasticQuotas map[string]*ElasticQuotaInfo
		want          []preemption.Candidate
	}{
		{
			name: "in-namespace preemption",
			pod:  makePod("t1-p", "ns1", 50, 0, 0, highPriority, "t1-p", ""),
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 50, 0, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 200,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			want: []preemption.Candidate{
				&candidate{
					victims: &extenderv1.Victims{
						Pods: []*v1.Pod{
							makePod("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
						},
						NumPDBViolations: 0,
					},
					name: "node-a",
				},
			},
		},
		{
			name: "cross-namespace preemption",
			pod:  makePod("t1-p", "ns1", 50, 0, 0, highPriority, "t1-p", ""),
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 0, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns2", 50, 0, 0, highPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 150,
					},
					Used: &framework.Resource{
						Memory: 50,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 200,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			want: []preemption.Candidate{
				&candidate{
					victims: &extenderv1.Victims{
						Pods: []*v1.Pod{
							makePod("t1-p3", "ns2", 50, 0, 0, midPriority, "t1-p3", "node-a"),
						},
						NumPDBViolations: 0,
					},
					name: "node-a",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := makeRegisteredPlugin()

			cs := clientsetfake.NewSimpleClientset()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fwk, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.pods, tt.nodes)),
				frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(cs, 0)),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()

			// Some tests rely on PreFilter plugin to compute its CycleState.
			_, preFilterStatus := fwk.RunPreFilterPlugins(ctx, state, tt.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}

			podReq := computePodResourceRequest(tt.pod)
			elasticQuotaSnapshotState := &ElasticQuotaSnapshotState{
				elasticQuotaInfos: tt.elasticQuotas,
			}
			prefilterState := &PreFilterState{
				podReq:                         *podReq,
				nominatedPodsReqWithPodReq:     *podReq,
				nominatedPodsReqInEQWithPodReq: *podReq,
			}
			state.Write(preFilterStateKey, prefilterState)
			state.Write(ElasticQuotaSnapshotKey, elasticQuotaSnapshotState)

			pe := preemption.Evaluator{
				PluginName: Name,
				Handler:    fwk,
				PodLister:  fwk.SharedInformerFactory().Core().V1().Pods().Lister(),
				PdbLister:  getPDBLister(fwk.SharedInformerFactory()),
				State:      state,
				Interface: &preemptor{
					fh:    fwk,
					state: state,
				},
			}

			nodeInfos, _ := fwk.SnapshotSharedLister().NodeInfos().List()
			got, _, err := pe.DryRunPreemption(ctx, tt.pod, nodeInfos, nil, 0, int32(len(nodeInfos)))
			if err != nil {
				t.Fatalf("unexpected error during DryRunPreemption(): %v", err)
			}

			// Sort the values (inner victims) and the candidate itself (by its NominatedNodeName).
			for i := range got {
				victims := got[i].Victims().Pods
				sort.Slice(victims, func(i, j int) bool {
					return victims[i].Name < victims[j].Name
				})
			}
			sort.Slice(got, func(i, j int) bool {
				return got[i].Name() < got[j].Name()
			})

			if len(got) != len(tt.want) {
				t.Fatalf("Unexpected candidate length: want %v, but bot %v", len(tt.want), len(got))
			}
			for i, c := range got {
				if diff := gocmp.Diff(c.Victims(), got[i].Victims()); diff != "" {
					t.Errorf("Unexpected victims at index %v (-want, +got): %s", i, diff)
				}
				if diff := gocmp.Diff(c.Name(), got[i].Name()); diff != "" {
					t.Errorf("Unexpected victims at index %v (-want, +got): %s", i, diff)
				}
			}
		})
	}
}

func TestPodEligibleToPreemptOthers(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150"}
	tests := []struct {
		name                string
		pod                 *v1.Pod
		existPods           []*v1.Pod
		nodes               []*v1.Node
		nominatedNodeStatus *framework.Status
		elasticQuotas       map[string]*ElasticQuotaInfo
		expected            bool
	}{
		{
			name:      "Pod with PreemptNever preemption policy",
			pod:       st.MakePod().Name("t1-p1").UID("t1-p1").Priority(highPriority).PreemptionPolicy(v1.PreemptNever).Obj(),
			existPods: []*v1.Pod{makePod("t1-p0", "ns1", 50, 10, 0, midPriority, "t1-p1", "node-a")},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, tainttoleration.ErrReasonNotMatch),
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 0,
					},
				},
			},
			expected: false,
		},
		{
			name:      "Pod with unschedulableAndUnresolvable nominated node",
			pod:       st.MakePod().Name("t1-p1").UID("t1-p1").Priority(highPriority).NominatedNodeName("node-a").Obj(),
			existPods: []*v1.Pod{makePod("t1-p0", "ns1", 50, 10, 0, midPriority, "t1-p1", "node-a")},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, tainttoleration.ErrReasonNotMatch),
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 0,
					},
				},
			},
			expected: true,
		},
		{
			name: "Pod with terminating pod in the same namespace",
			pod:  st.MakePod().Name("t1-p1").UID("t1-p1").Namespace("ns1").Priority(highPriority).NominatedNodeName("node-a").Obj(),
			existPods: []*v1.Pod{
				st.MakePod().Name("t1-p0").UID("t1-p0").Namespace("ns1").Priority(midPriority).Node("node-a").Terminating().Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: nil,
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 0,
					},
				},
			},
			expected: false,
		},
		{
			name: "Pod with terminating pod in another namespace",
			pod:  st.MakePod().Name("t1-p1").UID("t1-p1").Namespace("ns1").Priority(highPriority).NominatedNodeName("node-a").Obj(),
			existPods: []*v1.Pod{
				st.MakePod().Name("t1-p0").UID("t1-p0").Namespace("ns2").Priority(midPriority).Node("node-a").Terminating().Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: nil,
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 0,
					},
				},
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			expected: false,
		},
		{
			name: "Pod without elasticQuotas and terminating pod exists in the same namespace",
			pod:  st.MakePod().Name("t1-p1").UID("t1-p1").Namespace("ns1").Priority(highPriority).NominatedNodeName("node-a").Obj(),
			existPods: []*v1.Pod{
				st.MakePod().Name("t1-p0").UID("t1-p0").Namespace("ns2").Priority(midPriority).Node("node-a").Terminating().Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: nil,
			elasticQuotas:       map[string]*ElasticQuotaInfo{},
			expected:            false,
		},
		{
			name: "Pod without ElasticQuotas and terminating pod exists in another namespace",
			pod:  st.MakePod().Name("t1-p1").UID("t1-p1").Namespace("ns1").Priority(highPriority).NominatedNodeName("node-a").Obj(),
			existPods: []*v1.Pod{
				st.MakePod().Name("t1-p0").UID("t1-p0").Namespace("ns2").Priority(midPriority).Node("node-a").Terminating().Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			nominatedNodeStatus: nil,
			elasticQuotas: map[string]*ElasticQuotaInfo{
				"ns2": {
					Namespace: "ns2",
					Max: &framework.Resource{
						Memory: 150,
					},
					Min: &framework.Resource{
						Memory: 50,
					},
					Used: &framework.Resource{
						Memory: 100,
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := makeRegisteredPlugin()
			cs := clientsetfake.NewSimpleClientset()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fwk, err := tf.NewFramework(
				ctx,
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.existPods, tt.nodes)),
				frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(cs, 0)),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			_, preFilterStatus := fwk.RunPreFilterPlugins(ctx, state, tt.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}

			podReq := computePodResourceRequest(tt.pod)
			elasticQuotaSnapshotState := &ElasticQuotaSnapshotState{
				elasticQuotaInfos: tt.elasticQuotas,
			}
			prefilterState := &PreFilterState{
				podReq:                         *podReq,
				nominatedPodsReqWithPodReq:     *podReq,
				nominatedPodsReqInEQWithPodReq: *podReq,
			}
			state.Write(preFilterStateKey, prefilterState)
			state.Write(ElasticQuotaSnapshotKey, elasticQuotaSnapshotState)

			p := preemptor{fh: fwk, state: state}
			if got, _ := p.PodEligibleToPreemptOthers(tt.pod, tt.nominatedNodeStatus); got != tt.expected {
				t.Errorf("expected %t, got %t for pod: %s", tt.expected, got, tt.pod.Name)
			}
		})
	}
}

func TestAddElasticQuota(t *testing.T) {
	tests := []struct {
		name          string
		ns            []string
		elasticQuotas []*v1alpha1.ElasticQuota
		expected      map[string]*ElasticQuotaInfo
	}{
		{
			name: "Add ElasticQuota",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
					},
				},
			},
		},
		{
			name: "Add ElasticQuota without Max",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				makeEQ("ns1", "t1-eq1", nil, makeResourceList(10, 100)),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU:         UpperBoundOfMax,
						Memory:           UpperBoundOfMax,
						EphemeralStorage: UpperBoundOfMax,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
					},
				},
			},
		},
		{
			name: "Add ElasticQuota without Min",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), nil),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU:         LowerBoundOfMin,
						Memory:           LowerBoundOfMin,
						EphemeralStorage: LowerBoundOfMin,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
					},
				},
			},
		},
		{
			name: "Add ElasticQuota without Max and Min",
			elasticQuotas: []*v1alpha1.ElasticQuota{
				makeEQ("ns1", "t1-eq1", nil, nil),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU:         UpperBoundOfMax,
						Memory:           UpperBoundOfMax,
						EphemeralStorage: UpperBoundOfMax,
					},
					Min: &framework.Resource{
						MilliCPU:         LowerBoundOfMin,
						Memory:           LowerBoundOfMin,
						EphemeralStorage: LowerBoundOfMin,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}

			for _, elasticQuota := range tt.elasticQuotas {
				cs.addElasticQuota(elasticQuota)
			}

			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func TestUpdateElasticQuota(t *testing.T) {
	tests := []struct {
		name            string
		ns              []string
		oldElasticQuota *v1alpha1.ElasticQuota
		newElasticQuota *v1alpha1.ElasticQuota
		expected        map[string]*ElasticQuotaInfo
	}{
		{
			name:            "Update ElasticQuota without Used",
			oldElasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			newElasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(300, 1000), makeResourceList(10, 100)),
			ns:              []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU: 300,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}
			cs.addElasticQuota(tt.oldElasticQuota)
			cs.updateElasticQuota(tt.oldElasticQuota, tt.newElasticQuota)

			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func TestDeleteElasticQuota(t *testing.T) {
	tests := []struct {
		name         string
		ns           []string
		elasticQuota *v1alpha1.ElasticQuota
		expected     map[string]*ElasticQuotaInfo
	}{
		{
			name:         "Delete ElasticQuota",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(300, 1000), makeResourceList(10, 100)),
			ns:           []string{"ns1"},
			expected:     map[string]*ElasticQuotaInfo{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}
			cs.addElasticQuota(tt.elasticQuota)
			cs.deleteElasticQuota(tt.elasticQuota)

			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func TestAddPod(t *testing.T) {
	tests := []struct {
		name         string
		ns           []string
		elasticQuota *v1alpha1.ElasticQuota
		pods         []*v1.Pod
		expected     map[string]*ElasticQuotaInfo
	}{
		{
			name:         "AddPod",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			pods: []*v1.Pod{
				makePod("t1-p1", "ns1", 50, 10, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns1", 50, 10, 0, midPriority, "t1-p2", "node-a"),
				makePod("t1-p3", "ns1", 50, 10, 0, midPriority, "t1-p3", "node-a"),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.NewString("t1-p1", "t1-p2", "t1-p3"),
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 30,
						Memory:   150,
						ScalarResources: map[v1.ResourceName]int64{
							ResourceGPU: 0,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}
			cs.addElasticQuota(tt.elasticQuota)
			for _, pod := range tt.pods {
				cs.addPod(pod)
			}
			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func TestUpdatePod(t *testing.T) {
	tests := []struct {
		name         string
		ns           []string
		elasticQuota *v1alpha1.ElasticQuota
		updatePods   [][2]*v1.Pod
		expected     map[string]*ElasticQuotaInfo
	}{
		{
			name:         "Update Pod With Pod Status PodSucceeded and PodRunning",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			updatePods: [][2]*v1.Pod{
				{
					makePodWithStatus(makePod("t1-p1", "ns1", 100, 30, 0, midPriority, "t1-p1", "node-a"), v1.PodSucceeded),
					makePodWithStatus(makePod("t1-p1", "ns1", 100, 30, 0, highPriority, "t1-p1", "node-a"), v1.PodRunning),
				},
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.NewString("t1-p1"),
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 30,
						Memory:   100,
						ScalarResources: map[v1.ResourceName]int64{
							ResourceGPU: 0,
						},
					},
				},
			},
		},
		{
			name:         "Update Pod With Pod Status PodPending and PodFailed",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			updatePods: [][2]*v1.Pod{
				{
					makePodWithStatus(makePod("t1-p2", "ns1", 100, 30, 0, midPriority, "t1-p2", "node-a"), v1.PodPending),
					makePodWithStatus(makePod("t1-p2", "ns1", 100, 30, 0, highPriority, "t1-p2", "node-a"), v1.PodFailed),
				},
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.String{},
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
						ScalarResources: map[v1.ResourceName]int64{
							ResourceGPU: 0,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}
			cs.addElasticQuota(tt.elasticQuota)
			for _, pods := range tt.updatePods {
				cs.addPod(pods[0])
				cs.updatePod(pods[0], pods[1])
			}
			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func TestDeletePod(t *testing.T) {
	tests := []struct {
		name         string
		ns           []string
		elasticQuota *v1alpha1.ElasticQuota
		existingPods []*v1.Pod
		deletePods   []*v1.Pod
		expected     map[string]*ElasticQuotaInfo
	}{
		{
			name:         "Delete All Pods",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			existingPods: []*v1.Pod{
				makePod("t1-p1", "ns1", 100, 30, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns1", 100, 30, 0, highPriority, "t1-p2", "node-a"),
			},
			deletePods: []*v1.Pod{
				makePod("t1-p1", "ns1", 100, 30, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns1", 100, 30, 0, highPriority, "t1-p2", "node-a"),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.NewString(),
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 0,
						Memory:   0,
						ScalarResources: map[v1.ResourceName]int64{
							ResourceGPU: 0,
						},
					},
				},
			},
		},
		{
			name:         "Delete one Pod",
			elasticQuota: makeEQ("ns1", "t1-eq1", makeResourceList(100, 1000), makeResourceList(10, 100)),
			existingPods: []*v1.Pod{
				makePod("t1-p1", "ns1", 100, 30, 0, midPriority, "t1-p1", "node-a"),
				makePod("t1-p2", "ns1", 100, 30, 0, highPriority, "t1-p2", "node-a"),
			},
			deletePods: []*v1.Pod{
				makePod("t1-p1", "ns1", 100, 30, 0, midPriority, "t1-p1", "node-a"),
			},
			ns: []string{"ns1"},
			expected: map[string]*ElasticQuotaInfo{
				"ns1": {
					Namespace: "ns1",
					pods:      sets.NewString("t1-p2"),
					Max: &framework.Resource{
						MilliCPU: 100,
						Memory:   1000,
					},
					Min: &framework.Resource{
						MilliCPU: 10,
						Memory:   100,
					},
					Used: &framework.Resource{
						MilliCPU: 30,
						Memory:   100,
						ScalarResources: map[v1.ResourceName]int64{
							ResourceGPU: 0,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var registerPlugins []tf.RegisterPluginFunc
			registeredPlugins := append(
				registerPlugins,
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)

			fwk, err := tf.NewFramework(
				ctx, registeredPlugins, "",
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(nil)),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(make([]*v1.Pod, 0), make([]*v1.Node, 0))),
			)

			if err != nil {
				t.Fatal(err)
			}

			cs := &CapacityScheduling{
				elasticQuotaInfos: map[string]*ElasticQuotaInfo{},
				fh:                fwk,
			}
			cs.addElasticQuota(tt.elasticQuota)
			for _, existingpod := range tt.existingPods {
				cs.addPod(existingpod)
			}
			for _, deletepod := range tt.deletePods {
				cs.deletePod(deletepod)
			}
			for _, ns := range tt.ns {
				if got := cs.elasticQuotaInfos[ns]; !reflect.DeepEqual(got, tt.expected[ns]) {
					t.Errorf("expected %v, got %v", tt.expected[ns], got)
				}
			}
		})
	}
}

func makePod(podName string, namespace string, memReq int64, cpuReq int64, gpuReq int64, priority int32, uid string, nodeName string) *v1.Pod {
	pause := imageutils.GetPauseImageName()
	pod := st.MakePod().Namespace(namespace).Name(podName).Container(pause).
		Priority(priority).Node(nodeName).UID(uid).ZeroTerminationGracePeriod().Obj()
	pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(memReq, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewMilliQuantity(cpuReq, resource.DecimalSI),
			ResourceGPU:       *resource.NewQuantity(gpuReq, resource.DecimalSI),
		},
	}
	return pod
}

func makePodWithStatus(pod *v1.Pod, podPhase v1.PodPhase) *v1.Pod {
	pod.Status.Phase = podPhase
	return pod
}

func makeEQ(namespace, name string, max, min v1.ResourceList) *v1alpha1.ElasticQuota {
	eq := &v1alpha1.ElasticQuota{
		TypeMeta: metav1.TypeMeta{Kind: "ElasticQuota", APIVersion: "scheduling.sigs.k8s.io/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	eq.Spec.Max = max
	eq.Spec.Min = min
	return eq
}

func makeResourceList(cpu, mem int64) v1.ResourceList {
	return v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpu, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(mem, resource.BinarySI),
	}
}

func makeRegisteredPlugin() []tf.RegisterPluginFunc {
	registeredPlugins := []tf.RegisterPluginFunc{
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		tf.RegisterPluginAsExtensions(noderesources.Name, func(ctx context.Context, plArgs apiruntime.Object, fh framework.Handle) (framework.Plugin, error) {
			return noderesources.NewFit(ctx, plArgs, fh, plfeature.Features{})
		}, "Filter", "PreFilter"),
	}
	return registeredPlugins
}
