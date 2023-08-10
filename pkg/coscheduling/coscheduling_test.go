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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/utils/pointer"

	_ "sigs.k8s.io/scheduler-plugins/apis/config/scheme"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling/core"
	fakepgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPodGroupBackoffTime(t *testing.T) {
	tests := []struct {
		name string
		pod1 *v1.Pod
		pod2 *v1.Pod
		pod3 *v1.Pod
	}{
		{
			name: "pod in infinite scheduling loop.",
			pod1: st.MakePod().Name("pod1").UID("pod1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pod2: st.MakePod().Name("pod2").UID("pod2").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pod3: st.MakePod().Name("pod3").UID("pod3").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
		},
	}
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())
	pg1 := testutil.MakePG("pg1", "ns1", 3, nil, nil)
	pgInformer.Informer().GetStore().Add(pg1)

	fakeClient := clientsetfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	informerFactory.Start(ctx.Done())
	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{"test": "a"}, 60, 30)
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	// Compose a framework handle.
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
	f, err := st.NewFramework(registeredPlugins, "", ctx.Done(),
		frameworkruntime.WithClientSet(fakeClient),
		frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
	)
	if err != nil {
		t.Fatal(err)
	}
	scheduleDuration := 10 * time.Second
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInformer.Informer().GetStore().Add(tt.pod1)
			podInformer.Informer().GetStore().Add(tt.pod2)
			podInformer.Informer().GetStore().Add(tt.pod3)
			pgMgr := core.NewPodGroupManager(cs, snapshot, &scheduleDuration, pgInformer, podInformer)
			coscheduling := &Coscheduling{pgMgr: pgMgr, frameworkHandler: f, scheduleTimeout: &scheduleDuration, pgBackoff: pointer.Duration(time.Duration(1 * time.Second))}
			state := framework.NewCycleState()
			state.Write(framework.PodsToActivateKey, framework.NewPodsToActivate())
			code, _ := coscheduling.Permit(context.Background(), state, tt.pod1, "test")
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

			var expectPodsToActivate = map[string]*v1.Pod{
				"ns1/pod2": tt.pod2, "ns1/pod3": tt.pod3,
			}
			if !reflect.DeepEqual(expectPodsToActivate, podsToActive.Map) {
				t.Errorf("expected %v, got %v", expectPodsToActivate, podsToActive.Map)
				return
			}

			coscheduling.PostFilter(context.Background(), framework.NewCycleState(), tt.pod2, nil)

			_, code = coscheduling.PreFilter(context.Background(), framework.NewCycleState(), tt.pod3)
			if code.Code() != framework.UnschedulableAndUnresolvable {
				t.Errorf("expected %v, got %v", framework.UnschedulableAndUnresolvable, code.Code())
				return
			}
			pgFullName, _ := pgMgr.GetPodGroup(tt.pod1)
			if code.Reasons()[0] != fmt.Sprintf("podGroup %v failed recently", pgFullName) {
				t.Errorf("expected %v, got %v", pgFullName, code.Reasons()[0])
				return
			}
		})
	}
}

func TestLess(t *testing.T) {
	now := time.Now()
	times := make([]time.Time, 0)
	for _, d := range []time.Duration{0, 1, 2, 3, -2, -1} {
		times = append(times, now.Add(d*time.Second))
	}
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())
	for _, pgInfo := range []struct {
		createTime time.Time
		pgNme      string
		ns         string
		minMember  int32
	}{
		{
			createTime: times[2],
			pgNme:      "pg1",
			ns:         "namespace1",
		},
		{
			createTime: times[3],
			pgNme:      "pg2",
			ns:         "namespace2",
		},
		{
			createTime: times[4],
			pgNme:      "pg3",
			ns:         "namespace2",
		},
		{
			createTime: times[5],
			pgNme:      "pg4",
			ns:         "namespace2",
		},
	} {
		pg := testutil.MakePG(pgInfo.pgNme, pgInfo.ns, 5, &pgInfo.createTime, nil)
		pgInformer.Informer().GetStore().Add(pg)
	}

	fakeClient := clientsetfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	informerFactory.Start(ctx.Done())

	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{"test": "a"}, 60, 30)
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	scheduleDuration := 10 * time.Second
	var lowPriority, highPriority = int32(10), int32(100)
	ns1, ns2 := "namespace1", "namespace2"
	for _, tt := range []struct {
		name     string
		p1       *framework.QueuedPodInfo
		p2       *framework.QueuedPodInfo
		expected bool
	}{
		{
			name: "p1.priority less than p2.priority",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(lowPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(lowPriority).Obj()),
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority less than p2.priority, p1 belongs to podGroup1",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(lowPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to podGroup1",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(lowPriority).Obj()),
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup3",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg3").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to podGroup3",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg3").Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},

		{
			name: "p1.priority less than p2.priority, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(lowPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(lowPriority).Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to podGroup4 and p2 belongs to podGroup3",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg4").Obj()),
				InitialAttemptTimestamp: &times[1],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg3").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "equal priority and creation time, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg1").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority and creation time, p2 belong to podGroup2",
			p1: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).Name("pod1").Priority(highPriority).Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			p2: &framework.QueuedPodInfo{
				PodInfo:                 testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).Name("pod2").Priority(highPriority).Label(v1alpha1.PodGroupLabel, "pg2").Obj()),
				InitialAttemptTimestamp: &times[0],
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pgMgr := core.NewPodGroupManager(cs, snapshot, &scheduleDuration, pgInformer, podInformer)
			coscheduling := &Coscheduling{pgMgr: pgMgr}
			if got := coscheduling.Less(tt.p1, tt.p2); got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestPermit(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected framework.Code
	}{
		{
			name:     "pods do not belong to any podGroup",
			pod:      st.MakePod().Name("pod1").UID("pod1").Obj(),
			expected: framework.Success,
		},
		{
			name:     "pods belong to a podGroup, Wait",
			pod:      st.MakePod().Name("pod1").Namespace("ns1").UID("pod1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			expected: framework.Wait,
		},
		{
			name:     "pods belong to a podGroup, Allow",
			pod:      st.MakePod().Name("pod1").Namespace("ns1").UID("pod1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			expected: framework.Success,
		},
	}
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())
	pg1 := testutil.MakePG("pg1", "ns1", 2, nil, nil)
	pg2 := testutil.MakePG("pg2", "ns1", 1, nil, nil)
	pgInformer.Informer().GetStore().Add(pg1)
	pgInformer.Informer().GetStore().Add(pg2)

	fakeClient := clientsetfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	informerFactory.Start(ctx.Done())
	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{"test": "a"}, 60, 30)
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	// Compose a framework handle.
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
	f, err := st.NewFramework(registeredPlugins, "", ctx.Done(),
		frameworkruntime.WithClientSet(fakeClient),
		frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
	)
	if err != nil {
		t.Fatal(err)
	}
	scheduleDuration := 10 * time.Second
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgMgr := core.NewPodGroupManager(cs, snapshot, &scheduleDuration, pgInformer, podInformer)
			coscheduling := &Coscheduling{pgMgr: pgMgr, frameworkHandler: f, scheduleTimeout: &scheduleDuration}
			code, _ := coscheduling.Permit(context.Background(), framework.NewCycleState(), tt.pod, "test")
			if code.Code() != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, code.Code())
			}
		})
	}
}

func TestPostFilter(t *testing.T) {
	nodeStatusMap := framework.NodeToStatusMap{"node1": framework.NewStatus(framework.Success, "")}
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()
	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())
	pg := testutil.MakePG("pg", "ns1", 2, nil, nil)
	pgInformer.Informer().GetStore().Add(pg)
	fakeClient := clientsetfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	informerFactory.Start(ctx.Done())

	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{"test": "a"}, 60, 30)
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	// Compose a framework handle.
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
	f, err := st.NewFramework(registeredPlugins, "", ctx.Done(),
		frameworkruntime.WithClientSet(fakeClient),
		frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
	)
	if err != nil {
		t.Fatal(err)
	}

	existingPods, allNodes = testutil.MakeNodesAndPods(map[string]string{v1alpha1.PodGroupLabel: "pg"}, 10, 30)
	for _, pod := range existingPods {
		pod.Namespace = "ns1"
	}
	groupPodSnapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	scheduleDuration := 10 * time.Second
	tests := []struct {
		name                 string
		pod                  *v1.Pod
		expectedEmptyMsg     bool
		snapshotSharedLister framework.SharedLister
	}{
		{
			name:             "pod does not belong to any pod group",
			pod:              st.MakePod().Name("pod1").Namespace("ns1").UID("pod1").Obj(),
			expectedEmptyMsg: false,
		},
		{
			name:                 "enough pods assigned, do not reject all",
			pod:                  st.MakePod().Name("pod1").Namespace("ns1").UID("pod1").Label(v1alpha1.PodGroupLabel, "pg").Obj(),
			expectedEmptyMsg:     true,
			snapshotSharedLister: groupPodSnapshot,
		},
		{
			name:             "pod failed at filter phase, reject all pods",
			pod:              st.MakePod().Name("pod1").Namespace("ns1").UID("pod1").Label(v1alpha1.PodGroupLabel, "pg").Obj(),
			expectedEmptyMsg: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cycleState := framework.NewCycleState()
			mgrSnapShot := snapshot
			if tt.snapshotSharedLister != nil {
				mgrSnapShot = tt.snapshotSharedLister
			}

			pgMgr := core.NewPodGroupManager(cs, mgrSnapShot, &scheduleDuration, pgInformer, podInformer)
			coscheduling := &Coscheduling{pgMgr: pgMgr, frameworkHandler: f, scheduleTimeout: &scheduleDuration}
			_, code := coscheduling.PostFilter(context.Background(), cycleState, tt.pod, nodeStatusMap)
			if code.Message() == "" != tt.expectedEmptyMsg {
				t.Errorf("expectedEmptyMsg %v, got %v", tt.expectedEmptyMsg, code.Message() == "")
			}
		})
	}
}
