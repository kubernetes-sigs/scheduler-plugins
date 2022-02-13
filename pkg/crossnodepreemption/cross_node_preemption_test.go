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

package crossnodepreemption

/*
import (
	"context"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	dp "k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/interpodaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

var (
	lowPriority, midPriority, highPriority = int32(0), int32(10), int32(100)
)

func TestFindCandidates(t *testing.T) {
	fooSelector := st.MakeLabelSelector().Exists("foo").Obj()
	onePodRes := map[v1.ResourceName]string{v1.ResourcePods: "1"}
	tests := []struct {
		name            string
		pod             *v1.Pod
		pods            []*v1.Pod
		nodes           []*v1.Node
		nodesStatuses   framework.NodeToStatusMap
		registerPlugins []st.RegisterPluginFunc
		want            []dp.Candidate
	}{
		{
			name: "resolve PodTopologySpread constraint",
			pod: st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, fooSelector).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("pod-x").UID("pod-x").Node("node-x").Priority(highPriority).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label("node", "node-x").Capacity(onePodRes).Obj(),
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
				"node-b": framework.NewStatus(framework.Unschedulable),
				"node-x": framework.NewStatus(framework.Unschedulable),
			},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, func(plArgs apiruntime.Object, fh framework.Handle) (framework.Plugin, error) {
					return noderesources.NewFit(plArgs, fh, plfeature.Features{})
				}, "Filter", "PreFilter"),
				st.RegisterPluginAsExtensions(podtopologyspread.Name, podtopologyspread.New, "PreFilter", "Filter"),
			},
			want: []dp.Candidate{
				&candidate{
					victims: []*v1.Pod{
						st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
						st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
					},
					name: "node-a",
				},
				&candidate{
					victims: []*v1.Pod{
						st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
						st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
					},
					name: "node-b",
				},
			},
		},
		{
			name: "resolve PodAntiAffinity constraint",
			pod: st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).
				PodAntiAffinityExists("foo", "zone", st.PodAntiAffinityWithRequiredReq).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
				st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
				st.MakePod().Name("pod-x").UID("pod-x").Node("node-x").Priority(highPriority).Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Label("zone", "zone1").Label("node", "node-a").Obj(),
				st.MakeNode().Name("node-b").Label("zone", "zone1").Label("node", "node-b").Obj(),
				st.MakeNode().Name("node-x").Label("zone", "zone2").Label("node", "node-x").Capacity(onePodRes).Obj(),
			},
			nodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
				"node-b": framework.NewStatus(framework.Unschedulable),
				"node-x": framework.NewStatus(framework.Unschedulable),
			},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, func(plArgs apiruntime.Object, fh framework.Handle) (framework.Plugin, error) {
					return noderesources.NewFit(plArgs, fh, plfeature.Features{})
				}, "Filter", "PreFilter"),
				st.RegisterPluginAsExtensions(interpodaffinity.Name, func(plArgs apiruntime.Object, fh framework.Handle) (framework.Plugin, error) {
					return interpodaffinity.New(plArgs, fh, feature.Features{})
				}, "PreFilter", "Filter"),
			},
			want: []dp.Candidate{
				&candidate{
					victims: []*v1.Pod{
						st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
						st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
					},
					name: "node-a",
				},
				&candidate{
					victims: []*v1.Pod{
						st.MakePod().Name("pod-a").UID("pod-a").Node("node-a").Label("foo", "").Obj(),
						st.MakePod().Name("pod-b").UID("pod-b").Node("node-b").Label("foo", "").Obj(),
					},
					name: "node-b",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := append(
				tt.registerPlugins,
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)
			cs := clientsetfake.NewSimpleClientset()
			fwk, err := st.NewFramework(
				registeredPlugins,
				"default-scheduler",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator()),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.pods, tt.nodes)),
				frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(cs, 0)),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			ctx := context.Background()
			// Some tests rely on PreFilter plugin to compute its CycleState.
			preFilterStatus := fwk.RunPreFilterPlugins(ctx, state, tt.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}

			got, status := FindCandidates(ctx, state, tt.pod, tt.nodesStatuses, fwk, fwk.SnapshotSharedLister().NodeInfos())
			if !status.IsSuccess() {
				t.Fatal(status.AsError())
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
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(candidate{})); diff != "" {
				t.Errorf("Unexpected candidates (-want, +got): %s", diff)
			}
		})
	}
}
*/
