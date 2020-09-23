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
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clicache "k8s.io/client-go/tools/cache"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	fakepgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPreFilter(t *testing.T) {
	ctx := context.Background()
	cs := fakepgclientset.NewSimpleClientset()

	pgInformerFactory := pgformers.NewSharedInformerFactory(cs, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())

	pg := testutil.MakePG("pg", "ns1", 2, nil)
	pg1 := testutil.MakePG("pg1", "ns1", 2, nil)
	pgInformer.Informer().GetStore().Add(pg)
	pgInformer.Informer().GetStore().Add(pg1)
	pgLister := pgInformer.Lister()
	denyCache := newCache()
	denyCache.SetDefault("ns1/pg1", "ns1/pg1")

	tests := []struct {
		name            string
		pod             *corev1.Pod
		pods            []*corev1.Pod
		lastDeniedPG    *gochache.Cache
		expectedSuccess bool
	}{
		{
			name: "pod does not belong to any pg",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(util.PodGroupLabel, "pg2").Obj(),
			},
			lastDeniedPG:    newCache(),
			expectedSuccess: true,
		},
		{
			name:            "pg was previously denied",
			pod:             st.MakePod().Name("p1").UID("p1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			lastDeniedPG:    denyCache,
			expectedSuccess: false,
		},
		{
			name:            "pod belongs to a non-existing pg",
			pod:             st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(util.PodGroupLabel, "pg-notexisting").Obj(),
			lastDeniedPG:    newCache(),
			expectedSuccess: true,
		},
		{
			name: "pod count less than minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(util.PodGroupLabel, "pg2").Obj(),
			},
			lastDeniedPG:    newCache(),
			expectedSuccess: false,
		},
		{
			name: "pod count equal minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			},
			lastDeniedPG:    newCache(),
			expectedSuccess: true,
		},
		{
			name: "pod count more minMember",
			pod:  st.MakePod().Name("p2").UID("p2").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			pods: []*corev1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
				st.MakePod().Name("pg3-1").UID("pg3-1").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			},
			lastDeniedPG:    newCache(),
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
			pgMgr := &PodGroupManager{pgLister: pgLister, lastDeniedPG: tt.lastDeniedPG, snapshotSharedLister: snapshot, podLister: podInformer.Lister()}
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
	pg := testutil.MakePG("pg", "ns1", 2, nil)
	pg1 := testutil.MakePG("pg1", "ns1", 2, nil)
	fakeClient := fakepgclientset.NewSimpleClientset(pg, pg1)

	pgInformerFactory := pgformers.NewSharedInformerFactory(fakeClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()
	pgInformerFactory.Start(ctx.Done())

	pgInformer.Informer().GetStore().Add(pg)
	pgInformer.Informer().GetStore().Add(pg1)
	pgLister := pgInformer.Lister()

	existingPods, allNodes := testutil.MakeNodesAndPods(map[string]string{util.PodGroupLabel: "pg1"}, 1, 1)
	existingPods[0].Spec.NodeName = allNodes[0].Name
	existingPods[0].Namespace = "ns1"
	snapshot := testutil.NewFakeSharedLister(existingPods, allNodes)
	timeout := 10 * time.Second
	tests := []struct {
		name     string
		pod      *corev1.Pod
		snapshot framework.SharedLister
		allow    bool
	}{
		{
			name:  "pod does not belong to any pg, allow",
			pod:   st.MakePod().Name("p").UID("p").Namespace("ns1").Obj(),
			allow: true,
		},
		{
			name:  "pod belongs to pg, a non-existing pg",
			pod:   st.MakePod().Name("p").UID("p").Namespace("ns1").Label(util.PodGroupLabel, "pg-noexist").Obj(),
			allow: false,
		},
		{
			name:     "pod belongs to a pg that doesn't have enough pods",
			pod:      st.MakePod().Name("p").UID("p").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			snapshot: testutil.NewFakeSharedLister([]*corev1.Pod{}, []*corev1.Node{}),
			allow:    false,
		},
		{
			name:     "pod belongs to a pg that has enough pods",
			pod:      st.MakePod().Name("p").UID("p").Namespace("ns1").Label(util.PodGroupLabel, "pg1").Obj(),
			snapshot: snapshot,
			allow:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pgMgr := &PodGroupManager{pgClient: fakeClient, pgLister: pgLister, scheduleTimeout: &timeout, snapshotSharedLister: tt.snapshot}
			allow, err := pgMgr.Permit(ctx, tt.pod, "test")
			if allow != tt.allow {
				t.Errorf("want %v, but got %v. err: %v", tt.allow, allow, err)
			}
		})
	}
}

func newCache() *gochache.Cache {
	return gochache.New(10*time.Second, 10*time.Second)
}
