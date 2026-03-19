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

package core

import (
	"context"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clicache "k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	tu "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPreFilter(t *testing.T) {
	scheduleTimeout := 10 * time.Second
	capacity := map[corev1.ResourceName]string{
		corev1.ResourceCPU: "4",
	}
	nodes := []*corev1.Node{
		st.MakeNode().Name("node-a").Capacity(capacity).Obj(),
		st.MakeNode().Name("node-b").Capacity(capacity).Obj(),
	}

	tests := []struct {
		name            string
		pod             *corev1.Pod
		pendingPods     []*corev1.Pod
		pgs             []*v1alpha1.PodGroup
		expectedSuccess bool
	}{
		{
			name: "pod does not belong to any pg",
			pod:  st.MakePod().Name("p").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p2").Namespace("ns").UID("p2").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(1).Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "pod belongs to a non-existent pg",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg-non-existent").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(1).Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "pod count less than minMember",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("pg2-1").Namespace("ns").UID("p2-1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			expectedSuccess: false,
		},
		{
			name: "pod count equal minMember",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "pod count more than minMember",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
				tu.MakePodGroup().Name("pg2").Namespace("ns").MinMember(2).Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "gated pods but still meeting quorum (minMember < replicas)",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").
					SchedulingGates([]string{"example-gate"}).Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "gated pods push group below quorum",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").
					SchedulingGates([]string{"example-gate"}).Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(3).Obj(),
			},
			expectedSuccess: false,
		},
		{
			name: "all sibling pods are gated",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").
					SchedulingGates([]string{"example-gate"}).Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").
					SchedulingGates([]string{"example-gate"}).Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			expectedSuccess: false,
		},
		{
			// Previously we defined 2 nodes, each with 4 cpus. Now the PodGroup's minResources req is 6 cpus.
			name: "cluster's resource satisfies minResource", // Although it'd fail in Filter()
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).
					MinResources(map[corev1.ResourceName]string{corev1.ResourceCPU: "6"}).Obj(),
			},
			expectedSuccess: true,
		},
		{
			// Previously we defined 2 nodes, each with 4 cpus. Now the PodGroup's minResources req is 10 cpus.
			name: "cluster's resource cannot satisfy minResource",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pendingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("p1c").Namespace("ns").UID("p1c").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).
					MinResources(map[corev1.ResourceName]string{corev1.ResourceCPU: "10"}).Obj(),
			},
			expectedSuccess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Compile all objects into `objs`.
			var objs []runtime.Object
			for _, pod := range append(tt.pendingPods, tt.pod) {
				objs = append(objs, pod)
			}
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

			pgMgr := &PodGroupManager{
				client:               client,
				snapshotSharedLister: tu.NewFakeSharedLister(tt.pendingPods, nodes),
				podLister:            podInformer.Lister(),
				scheduleTimeout:      &scheduleTimeout,
				permittedPG:          newCache(),
				backedOffPG:          newCache(),
				// lastFailedSchedulePG is a sync.Map, zero-value ready.
				assignedPodsByPG: make(map[string]sets.Set[string]),
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range append(tt.pendingPods, tt.pod) {
				podInformer.Informer().GetStore().Add(p)
			}

			err = pgMgr.PreFilter(ctx, tt.pod)
			if (err == nil) != tt.expectedSuccess {
				t.Errorf("Want %v, but got %v", tt.expectedSuccess, err == nil)
			}
		})
	}
}

func TestPermit(t *testing.T) {
	scheduleTimeout := 10 * time.Second
	capacity := map[corev1.ResourceName]string{
		corev1.ResourceCPU: "4",
	}
	nodes := []*corev1.Node{
		st.MakeNode().Name("node").Capacity(capacity).Obj(),
	}

	tests := []struct {
		name         string
		pod          *corev1.Pod
		existingPods []*corev1.Pod
		pgs          []*v1alpha1.PodGroup
		want         Status
	}{
		{
			name: "pod does not belong to any pg",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: PodGroupNotSpecified,
		},
		{
			name: "pod specifies a non-existent pg",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg-non-existent").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: PodGroupNotFound,
		},
		{
			name: "pod belongs to a pg that doesn't have quorum",
			pod:  st.MakePod().Name("p").Namespace("ns").UID("p").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: Wait,
		},
		{
			name: "pod belongs to a pg that have quorum satisfied",
			pod:  st.MakePod().Name("p1a").Namespace("ns").UID("p1a").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			existingPods: []*corev1.Pod{
				st.MakePod().Name("p1b").Namespace("ns").UID("p1b").Label(v1alpha1.PodGroupLabel, "pg1").Node("node").Obj(),
			},
			pgs: []*v1alpha1.PodGroup{
				tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj(),
			},
			want: Success,
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

			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()

			pgMgr := NewPodGroupManager(client, tu.NewFakeSharedLister(tt.existingPods, nodes), &scheduleTimeout, podInformer)

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			addFunc := AddPodFactory(pgMgr)
			for _, p := range tt.existingPods {
				podInformer.Informer().GetStore().Add(p)
				// we call add func here because we can not ensure existing pods are added before premit are called
				addFunc(p)
			}

			if got := pgMgr.Permit(ctx, &framework.CycleState{}, tt.pod); got != tt.want {
				t.Errorf("Want %v, but got %v", tt.want, got)
			}
		})
	}
}

func TestCheckClusterResource(t *testing.T) {
	capacity := map[corev1.ResourceName]string{
		corev1.ResourceCPU: "3",
	}
	// In total, the cluster has 3*2 = 6 cpus.
	nodes := []*corev1.Node{
		st.MakeNode().Name("node-a").Capacity(capacity).Obj(),
		st.MakeNode().Name("node-b").Capacity(capacity).Obj(),
	}

	tests := []struct {
		name         string
		existingPods []*corev1.Pod
		minResources corev1.ResourceList
		pgName       string
		want         bool
	}{
		{
			name: "Cluster resource enough",
			existingPods: []*corev1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Node("node-a").
					Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "2"}).Obj(),
			},
			minResources: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
			},
			pgName: "ns/pg1",
			want:   true,
		},
		{
			name: "Cluster resource not enough",
			existingPods: []*corev1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Node("node-a").
					Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "2"}).Obj(),
				st.MakePod().Name("p2").Namespace("ns").UID("p2").Node("node-b").
					Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "1"}).Obj(),
			},
			minResources: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
			},
			pgName: "ns/pg1",
			want:   false,
		},
		{
			name: "Cluster resource enough as p1's resource needs to be excluded from minResources",
			existingPods: []*corev1.Pod{
				st.MakePod().Name("p1").Namespace("ns").UID("p1").Label(v1alpha1.PodGroupLabel, "pg1").Node("node-a").
					Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "2"}).Obj(),
				st.MakePod().Name("p2").Namespace("ns").UID("p2").Node("node-b").
					Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "1"}).Obj(),
			},
			minResources: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
			},
			pgName: "ns/pg1",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshotSharedLister := tu.NewFakeSharedLister(tt.existingPods, nodes)
			nodeInfoList, _ := snapshotSharedLister.NodeInfos().List()
			err := CheckClusterResource(context.Background(), nodeInfoList, tt.minResources, tt.pgName)
			if (err == nil) != tt.want {
				t.Errorf("Expect the cluster resource to be satified: %v, but got %v", tt.want, err == nil)
			}
		})
	}
}

func TestMarkAndClearPodGroupScheduleFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now()
	// Truncate to seconds since metav1.Time serialization drops sub-second precision
	pgCreationTime := now.Add(-10 * time.Second).Truncate(time.Second)
	pg1 := tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(3).Time(pgCreationTime).Obj()

	var objs []runtime.Object
	objs = append(objs, pg1)

	client, err := tu.NewFakeClient(objs...)
	if err != nil {
		t.Fatal(err)
	}

	cs := clientsetfake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	podInformer := informerFactory.Core().V1().Pods()
	scheduleTimeout := 10 * time.Second

	pgMgr := NewPodGroupManager(client, nil, &scheduleTimeout, podInformer)

	informerFactory.Start(ctx.Done())
	if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
		t.Fatal("WaitForCacheSync failed")
	}

	pod := st.MakePod().Name("p1").Namespace("ns").Label(v1alpha1.PodGroupLabel, "pg1").Obj()

	// Before failure: should return PodGroup's creation timestamp
	ts1 := pgMgr.GetCreationTimestamp(ctx, pod, now)
	if !ts1.Equal(pg1.CreationTimestamp.Time) {
		t.Errorf("Expected PodGroup creation timestamp %v, got %v", pg1.CreationTimestamp.Time, ts1)
	}

	// Mark failure
	pgMgr.MarkPodGroupScheduleFailure("ns/pg1")

	// After failure: should return a timestamp after the PodGroup's creation timestamp
	ts2 := pgMgr.GetCreationTimestamp(ctx, pod, now)
	if !ts2.After(pg1.CreationTimestamp.Time) {
		t.Errorf("Expected failure timestamp %v to be after PodGroup creation timestamp %v", ts2, pg1.CreationTimestamp.Time)
	}

	// Clear failure
	pgMgr.ClearPodGroupScheduleFailure("ns/pg1")

	// After clear: should return PodGroup's creation timestamp again
	ts3 := pgMgr.GetCreationTimestamp(ctx, pod, now)
	if !ts3.Equal(pg1.CreationTimestamp.Time) {
		t.Errorf("Expected PodGroup creation timestamp %v after clear, got %v", pg1.CreationTimestamp.Time, ts3)
	}
}

func TestSchemeRestriction(t *testing.T) {
	pg := tu.MakePodGroup().Name("pg1").Namespace("ns").MinMember(2).Obj()

	// Build a client with ONLY v1alpha1 — matching the production coscheduling plugin.
	restrictedScheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(restrictedScheme); err != nil {
		t.Fatal(err)
	}
	restrictedClient := fake.NewClientBuilder().
		WithScheme(restrictedScheme).
		WithRuntimeObjects(pg).
		Build()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("allows PodGroup Get", func(t *testing.T) {
		var got v1alpha1.PodGroup
		if err := restrictedClient.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "pg1"}, &got); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		if got.Spec.MinMember != 2 {
			t.Errorf("MinMember = %d, want 2", got.Spec.MinMember)
		}
	})

	t.Run("allows PodGroup List", func(t *testing.T) {
		var list v1alpha1.PodGroupList
		if err := restrictedClient.List(ctx, &list); err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(list.Items) != 1 {
			t.Errorf("len(Items) = %d, want 1", len(list.Items))
		}
	})

	tests := []struct {
		name string
		obj  runtime.Object
	}{
		{"blocks Pod Get", &corev1.Pod{}},
		{"blocks Pod List", &corev1.PodList{}},
		{"blocks Node Get", &corev1.Node{}},
		{"blocks Service Get", &corev1.Service{}},
		{"blocks ConfigMap Get", &corev1.ConfigMap{}},
		{"blocks Secret Get", &corev1.Secret{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := restrictedScheme.ObjectKinds(tt.obj)
			if err == nil {
				t.Errorf("scheme resolved %T, want error (type should not be registered)", tt.obj)
			}
		})
	}

	t.Run("GetPodGroup end-to-end", func(t *testing.T) {
		cs := clientsetfake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactory(cs, 0)
		podInformer := informerFactory.Core().V1().Pods()
		scheduleTimeout := 10 * time.Second

		pgMgr := NewPodGroupManager(restrictedClient, tu.NewFakeSharedLister(nil, nil), &scheduleTimeout, podInformer)
		informerFactory.Start(ctx.Done())
		if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
			t.Fatal("WaitForCacheSync failed")
		}

		pod := st.MakePod().Name("p1").Namespace("ns").UID("p1").Label(v1alpha1.PodGroupLabel, "pg1").Obj()
		pgName, pgObj := pgMgr.GetPodGroup(ctx, pod)
		if pgObj == nil {
			t.Fatal("GetPodGroup returned nil")
		}
		if pgName != "ns/pg1" {
			t.Errorf("pgName = %q, want %q", pgName, "ns/pg1")
		}
		if pgObj.Spec.MinMember != 2 {
			t.Errorf("MinMember = %d, want 2", pgObj.Spec.MinMember)
		}
	})
}

func newCache() *gocache.Cache {
	return gocache.New(10*time.Second, 10*time.Second)
}
