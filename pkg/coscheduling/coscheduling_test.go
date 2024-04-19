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

package coscheduling

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clicache "k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/utils/pointer"

	_ "sigs.k8s.io/scheduler-plugins/apis/config/scheme"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPodGroupBackoffTime(t *testing.T) {
	scheduleDuration := 10 * time.Second
	capacity := map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}
	nodes := []*v1.Node{
		st.MakeNode().Name("node").Capacity(capacity).Obj(),
	}

	tests := []struct {
		name              string
		pods              []*v1.Pod
		pgs               []*v1alpha1.PodGroup
		wantActivatedPods []string
		want              framework.Code
	}{
		{
			name: "prevent pod falling into infinite scheduling loop",
			pods: []*v1.Pod{
				st.MakePod().Name("pod1").UID("pod1").Namespace("ns").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pod2").UID("pod2").Namespace("ns").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pod3").UID("pod3").Namespace("ns").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(3).Obj(),
			},
			wantActivatedPods: []string{"ns/pod2", "ns/pod3"},
			want:              framework.UnschedulableAndUnresolvable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Compile all objects into `objs`.
			var objs []runtime.Object
			for _, pod := range tt.pods {
				objs = append(objs, pod)
			}
			for _, pg := range tt.pgs {
				objs = append(objs, pg)
			}

			client, err := tu.NewFakeClient(objs...)
			if err != nil {
				t.Fatal(err)
			}

			// Compose a fake framework handle.
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()
			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}
			f, err := tf.NewFramework(ctx, registeredPlugins, "default-scheduler", fwkruntime.WithInformerFactory(informerFactory))
			if err != nil {
				t.Fatal(err)
			}

			pgMgr := core.NewPodGroupManager(
				client,
				tu.NewFakeSharedLister(tt.pods, nodes),
				// In this UT, 5 seconds should suffice to test the PreFilter's return code.
				pointer.Duration(5*time.Second),
				podInformer,
			)
			pl := &Coscheduling{
				frameworkHandler: f,
				pgMgr:            pgMgr,
				scheduleTimeout:  &scheduleDuration,
				pgBackoff:        pointer.Duration(1 * time.Second),
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range tt.pods {
				podInformer.Informer().GetStore().Add(p)
			}

			state := framework.NewCycleState()
			state.Write(framework.PodsToActivateKey, framework.NewPodsToActivate())
			code, _ := pl.Permit(ctx, state, tt.pods[0], "test")
			if code.Code() != framework.Wait {
				t.Errorf("expected %v, got %v", framework.Wait, code.Code())
				return
			}

			podsToActiveObj, err := state.Read(framework.PodsToActivateKey)
			if err != nil {
				t.Errorf("expecte pod2 and pod3 in pods to active")
				return
			}
			podsToActive, ok := podsToActiveObj.(*framework.PodsToActivate)
			if !ok {
				t.Errorf("cannot convert type %t to *framework.PodsToActivate", podsToActiveObj)
				return
			}
			var got []string
			for podName := range podsToActive.Map {
				got = append(got, podName)
			}
			sort.Strings(got)
			if diff := cmp.Diff(got, tt.wantActivatedPods); diff != "" {
				t.Errorf("unexpected activatedPods (-want, +got): %s\n", diff)
				return
			}

			pl.PostFilter(ctx, framework.NewCycleState(), tt.pods[1], nil)

			_, code = pl.PreFilter(ctx, framework.NewCycleState(), tt.pods[2])
			if code.Code() != tt.want {
				t.Errorf("expected %v, got %v", tt.want, code.Code())
				return
			}
			pgFullName, _ := pgMgr.GetPodGroup(ctx, tt.pods[0])
			if code.Reasons()[0] != fmt.Sprintf("podGroup %v failed recently", pgFullName) {
				t.Errorf("expected %v, got %v", pgFullName, code.Reasons()[0])
				return
			}
		})
	}
}

func ptrTime(tm time.Time) *time.Time {
	return &tm
}

func TestLess(t *testing.T) {
	lowPriority, highPriority := int32(10), int32(100)
	now := time.Now()

	tests := []struct {
		name string
		p1   *framework.QueuedPodInfo
		p2   *framework.QueuedPodInfo
		pgs  []*v1alpha1.PodGroup
		want bool
	}{
		{
			name: "p1.priority less than p2.priority",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(lowPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
			},
			want: false,
		},
		{
			name: "p1.priority greater than p2.priority",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(lowPriority).Obj()),
			},
			want: true,
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			want: true,
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			want: false,
		},
		{
			name: "p1.priority less than p2.priority, p1 belongs to pg1",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(lowPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Obj(),
			},
			want: false,
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to pg1",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(lowPriority).Obj()),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Obj(),
			},
			want: true,
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to pg1",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 1)).Obj(),
			},
			want: true,
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to pg1",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: false,
		},
		{
			name: "p1.priority less than p2.priority, p1 belongs to pg1 and p2 belongs to pg2",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(lowPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 1)).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns2").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: false,
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to pg1 and and p2 belongs to pg2",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(lowPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 1)).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns2").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: true,
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to pg1 and p2 belongs to pg2",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 1)).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns2").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: true,
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to pg2 and p2 belongs to pg1",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns").Priority(lowPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 2)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").Time(now.Add(time.Second * 1)).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: false,
		},
		{
			name: "equal priority and creation time, p1 belongs pg1 and p2 belong to pg2",
			p1: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns1").Time(now.Add(time.Second * 1)).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns2").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: true,
		},
		{
			name: "equal priority and creation time, and p2 belong to pg2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 tu.MustNewPodInfo(t, st.MakePod().Name("p1").Namespace("ns1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: tu.MustNewPodInfo(t, st.MakePod().Name("p2").Namespace("ns2").Priority(highPriority).
					Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: ptrTime(now.Add(time.Second * 1)),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg2").Namespace("ns2").Time(now.Add(time.Second * 2)).Obj(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Compile all objects into `objs`.
			var objs []runtime.Object
			for _, pg := range tt.pgs {
				objs = append(objs, pg)
			}

			client, err := tu.NewFakeClient(objs...)
			if err != nil {
				t.Fatal(err)
			}
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()

			pl := &Coscheduling{pgMgr: core.NewPodGroupManager(client, nil, nil, podInformer)}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}

			if got := pl.Less(tt.p1, tt.p2); got != tt.want {
				t.Errorf("Want %v, got %v", tt.want, got)
			}
		})
	}
}

func TestPermit(t *testing.T) {
	scheduleTimeout := 10 * time.Second
	capacity := map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}
	nodes := []*v1.Node{
		st.MakeNode().Name("node").Capacity(capacity).Obj(),
	}

	tests := []struct {
		name string
		pod  *v1.Pod
		pgs  []*v1alpha1.PodGroup
		want framework.Code
	}{
		{
			name: "pods do not belong to any podGroup",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			want: framework.Success,
		},
		{
			name: "pods belong to a pg1, but quorum not satisfied",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			want: framework.Wait,
		},
		{
			name: "pods belong to a podGroup, and quorum satisfied",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			want: framework.Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Compile all objects into `objs`.
			var objs []runtime.Object
			objs = append(objs, tt.pod)
			for _, pg := range tt.pgs {
				objs = append(objs, pg)
			}

			client, err := tu.NewFakeClient(objs...)
			if err != nil {
				t.Fatal(err)
			}

			// Compose a fake framework handle.
			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}
			f, err := tf.NewFramework(ctx, registeredPlugins, "default-scheduler")
			if err != nil {
				t.Fatal(err)
			}
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()

			pl := &Coscheduling{
				frameworkHandler: f,
				pgMgr:            core.NewPodGroupManager(client, tu.NewFakeSharedLister(nil, nodes), nil, podInformer),
				scheduleTimeout:  &scheduleTimeout,
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}

			code, _ := pl.Permit(ctx, framework.NewCycleState(), tt.pod, "node")
			if got := code.Code(); got != tt.want {
				t.Errorf("Want %v, but got %v", tt.want, got)
			}
		})
	}
}

func TestPostFilter(t *testing.T) {
	scheduleTimeout := 10 * time.Second
	capacity := map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}
	nodes := []*v1.Node{
		st.MakeNode().Name("node").Capacity(capacity).Obj(),
	}
	nodeStatusMap := framework.NodeToStatusMap{"node": framework.NewStatus(framework.Success, "")}

	tests := []struct {
		name         string
		pod          *v1.Pod
		existingPods []*v1.Pod
		pgs          []*v1alpha1.PodGroup
		want         *framework.Status
	}{
		{
			name: "pod does not belong to any pod group",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: framework.NewStatus(framework.Unschedulable, "can not find pod group"),
		},
		{
			name: "enough pods assigned, do not reject all",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Node("node").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
			},
			want: framework.NewStatus(framework.Unschedulable),
		},
		{
			name: "pod failed at filter phase, reject all pods",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			existingPods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Node("node").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: framework.NewStatus(
				framework.Unschedulable,
				"PodGroup ns/pg1 gets rejected due to Pod p is unschedulable even after PostFilter",
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Compile all objects into `objs`.
			var objs []runtime.Object
			for _, pod := range append(tt.existingPods, tt.pod) {
				objs = append(objs, pod)
			}
			for _, pg := range tt.pgs {
				objs = append(objs, pg)
			}

			client, err := tu.NewFakeClient(objs...)
			if err != nil {
				t.Fatal(err)
			}

			// Compose a fake framework handle.
			registeredPlugins := []tf.RegisterPluginFunc{
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}
			f, err := tf.NewFramework(ctx, registeredPlugins, "default-scheduler")
			if err != nil {
				t.Fatal(err)
			}

			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()

			pl := &Coscheduling{
				frameworkHandler: f,
				pgMgr: core.NewPodGroupManager(
					client,
					tu.NewFakeSharedLister(tt.existingPods, nodes),
					&scheduleTimeout,
					podInformer,
				),
				scheduleTimeout: &scheduleTimeout,
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range tt.existingPods {
				podInformer.Informer().GetStore().Add(p)
			}

			_, got := pl.PostFilter(ctx, framework.NewCycleState(), tt.pod, nodeStatusMap)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Want %v, but got %v", tt.want, got)
			}
		})
	}
}
