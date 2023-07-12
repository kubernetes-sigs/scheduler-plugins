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
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clicache "k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	fakepgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPreFilter(t *testing.T) {
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()

	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())
	scheduleTimeout := 10 * time.Second
	pg := testutil.MakePG("pg", "ns1", 2, nil, nil)
	pg1 := testutil.MakePG("pg1", "ns1", 2, nil, nil)
	pg2 := testutil.MakePG("pg2", "ns1", 2, nil, &corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")})
	pg3 := testutil.MakePG("pg3", "ns1", 2, nil, &corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("40")})
	pgInformer.Informer().GetStore().Add(pg)
	pgInformer.Informer().GetStore().Add(pg1)
	pgInformer.Informer().GetStore().Add(pg2)
	pgInformer.Informer().GetStore().Add(pg3)
	pgLister := pgInformer.Lister()

	tests := []struct {
		name            string
		pod             *corev1.Pod
		pods            []*corev1.Pod
		expectedSuccess bool
	}{
		{
			name: "pod does not belong to any pg",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			},
			expectedSuccess: true,
		},
		{
			name:            "pod belongs to a non-existing pg",
			pod:             st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg-notexisting").Obj(),
			expectedSuccess: true,
		},
		{
			name: "pod count less than minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			},
			expectedSuccess: false,
		},
		{
			name: "pod count equal minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "pod count more minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg3-1").UID("pg3-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "cluster resource enough, min Resource",
			pod: st.MakePod().Name("p2-1").UID("p2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg2").
				Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "1"}).Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg2").Obj(),
			},
			expectedSuccess: true,
		},
		{
			name: "cluster resource not enough, min Resource",
			pod: st.MakePod().Name("p2-1").UID("p2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg3").
				Req(map[corev1.ResourceName]string{corev1.ResourceCPU: "20"}).Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg3").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg3").Obj(),
			},
			expectedSuccess: false,
		},
		{
			name: "cluster resource enough not required",
			pod:  st.MakePod().Name("p2-1").UID("p2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			},
			expectedSuccess: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()
			existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{"test": "a"}, 60, 30)
			snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
			pgMgr := &PodGroupManager{pgLister: pgLister, permittedPG: newCache(),
				snapshotSharedLister: snapshot, podLister: podInformer.Lister(), scheduleTimeout: &scheduleTimeout, backedOffPG: gochache.New(10*time.Second, 10*time.Second)}
			informerFactory.Start(ctx.Done())
			if !clicache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced) {
				t.Fatal("WaitForCacheSync failed")
			}
			for _, p := range tt.pods {
				podInformer.Informer().GetStore().Add(p)
			}
			err := pgMgr.PreFilter(ctx, tt.pod)
			if (err == nil) != tt.expectedSuccess {
				t.Errorf("desire %v, get %v", tt.expectedSuccess, err == nil)
			}
		})
	}
}

func TestPermit(t *testing.T) {
	ctx := context.Background()
	pg := testutil.MakePG("pg", "ns1", 2, nil, nil)
	pg1 := testutil.MakePG("pg1", "ns1", 2, nil, nil)
	fakeClient := fakepgclientset.NewSimpleClientset(pg, pg1)

	pgInformerFactory := pgformers.NewSharedInformerFactory(fakeClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())

	pgInformer.Informer().GetStore().Add(pg)
	pgInformer.Informer().GetStore().Add(pg1)
	pgLister := pgInformer.Lister()

	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{v1alpha1.PodGroupLabel: "pg1"}, 1, 1)
	existingPods[0].Spec.NodeName = allNodes[0].Name
	existingPods[0].Namespace = "ns1"
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	timeout := 10 * time.Second
	tests := []struct {
		name     string
		pod      *corev1.Pod
		snapshot framework.SharedLister
		want     Status
	}{
		{
			name: "pod does not belong to any pg, allow",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Obj(),
			want: PodGroupNotSpecified,
		},
		{
			name: "pod belongs to a non-existing pg",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg-noexist").Obj(),
			want: PodGroupNotFound,
		},
		{
			name:     "pod belongs to a pg that doesn't have enough pods",
			pod:      st.MakePod().Name("p").UID("p").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			snapshot: testutil.NewFakeSharedLister([]*corev1.Pod{}, []*corev1.Node{}),
			want:     Wait,
		},
		{
			name:     "pod belongs to a pg that has enough pods",
			pod:      st.MakePod().Name("p").UID("p").Namespace("ns1").Label(v1alpha1.PodGroupLabel, "pg1").Obj(),
			snapshot: snapshot,
			want:     Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgMgr := &PodGroupManager{pgClient: fakeClient, pgLister: pgLister, scheduleTimeout: &timeout, snapshotSharedLister: tt.snapshot}
			if got := pgMgr.Permit(ctx, tt.pod); got != tt.want {
				t.Errorf("Expect %v, but got %v", tt.want, got)
			}
		})
	}
}

func TestCheckClusterResource(t *testing.T) {
	nodeRes := map[corev1.ResourceName]string{corev1.ResourceMemory: "300"}
	node := st.MakeNode().Name("fake-node").Capacity(nodeRes).Obj()
	snapshot := testutil.NewFakeSharedLister(nil, []*corev1.Node{node})
	nodeInfo, _ := snapshot.NodeInfos().List()

	pod := st.MakePod().Name("t1-p1-3").Req(map[corev1.ResourceName]string{corev1.ResourceMemory: "100"}).Label(v1alpha1.PodGroupLabel,
		"pg1-1").ZeroTerminationGracePeriod().Obj()
	snapshotWithAssumedPod := testutil.NewFakeSharedLister([]*corev1.Pod{pod}, []*corev1.Node{node})
	scheduledNodeInfo, _ := snapshotWithAssumedPod.NodeInfos().List()
	tests := []struct {
		name                  string
		resourceRequest       corev1.ResourceList
		desiredPGName         string
		nodeList              []*framework.NodeInfo
		desiredResourceEnough bool
	}{
		{
			name: "Cluster resource enough",
			resourceRequest: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(10, resource.DecimalSI),
			},
			nodeList:              nodeInfo,
			desiredResourceEnough: true,
		},
		{
			name: "Cluster resource not enough",
			resourceRequest: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(1000, resource.DecimalSI),
			},
			nodeList:              nodeInfo,
			desiredResourceEnough: false,
		},
		{
			name: "Cluster resource enough, some resources of the pods belonging to the group have been included",
			resourceRequest: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(250, resource.DecimalSI),
			},
			nodeList:              scheduledNodeInfo,
			desiredResourceEnough: true,
			desiredPGName:         "pg1-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckClusterResource(tt.nodeList, tt.resourceRequest, tt.desiredPGName)
			if (err == nil) != tt.desiredResourceEnough {
				t.Errorf("want resource enough %v, but got %v", tt.desiredResourceEnough, err != nil)
			}
		})
	}

}

func newCache() *gochache.Cache {
	return gochache.New(10*time.Second, 10*time.Second)
}
