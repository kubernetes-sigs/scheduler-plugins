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

	gochache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clicache "k8s.io/client-go/tools/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
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
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range tt.pendingPods {
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

			pgMgr := &PodGroupManager{
				client:               client,
				snapshotSharedLister: tu.NewFakeSharedLister(tt.existingPods, nodes),
				podLister:            podInformer.Lister(),
				scheduleTimeout:      &scheduleTimeout,
			}

			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range tt.existingPods {
				podInformer.Informer().GetStore().Add(p)
			}

			if got := pgMgr.Permit(ctx, tt.pod); got != tt.want {
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
			err := CheckClusterResource(nodeInfoList, tt.minResources, tt.pgName)
			if (err == nil) != tt.want {
				t.Errorf("Expect the cluster resource to be satified: %v, but got %v", tt.want, err == nil)
			}
		})
	}
}

func newCache() *gochache.Cache {
	return gochache.New(10*time.Second, 10*time.Second)
}
