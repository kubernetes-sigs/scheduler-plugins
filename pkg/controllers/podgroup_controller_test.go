/*
Copyright 2023 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/klogr"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

func Test_Run(t *testing.T) {
	ctx := context.TODO()
	createTime := metav1.Time{Time: time.Now().Add(-72 * time.Hour)}
	cases := []struct {
		name               string
		pgName             string
		minMember          int32
		podNames           []string
		podNextPhase       v1.PodPhase
		podPhase           v1.PodPhase
		previousPhase      v1alpha1.PodGroupPhase
		desiredGroupPhase  v1alpha1.PodGroupPhase
		podGroupCreateTime *metav1.Time
	}{
		{
			name:              "Group running",
			pgName:            "pg1",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodRunning,
			previousPhase:     v1alpha1.PodGroupScheduling,
			desiredGroupPhase: v1alpha1.PodGroupRunning,
		},
		{
			name:              "Group running, more than min member",
			pgName:            "pg11",
			minMember:         2,
			podNames:          []string{"pod11", "pod21", "pod31", "pod41"},
			podPhase:          v1.PodRunning,
			previousPhase:     v1alpha1.PodGroupScheduling,
			desiredGroupPhase: v1alpha1.PodGroupRunning,
		},
		{
			name:              "Group failed",
			pgName:            "pg2",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodFailed,
			previousPhase:     v1alpha1.PodGroupScheduling,
			desiredGroupPhase: v1alpha1.PodGroupFailed,
		},
		{
			name:              "Group finished",
			pgName:            "pg3",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodSucceeded,
			previousPhase:     v1alpha1.PodGroupScheduling,
			desiredGroupPhase: v1alpha1.PodGroupFinished,
		},
		{
			name:              "Group status convert from pending to prescheduling",
			pgName:            "pg4",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodPending,
			previousPhase:     v1alpha1.PodGroupPending,
			desiredGroupPhase: v1alpha1.PodGroupScheduling,
		},
		{
			name:              "Group status convert from scheduling to finished",
			pgName:            "pg5",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodPending,
			previousPhase:     v1alpha1.PodGroupScheduling,
			desiredGroupPhase: v1alpha1.PodGroupFinished,
			podNextPhase:      v1.PodSucceeded,
		},
		{
			name:              "Group status keeps pending",
			pgName:            "pg6",
			minMember:         3,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodRunning,
			previousPhase:     v1alpha1.PodGroupPending,
			desiredGroupPhase: v1alpha1.PodGroupPending,
		},
		{
			name:              "Group status convert from pending to finished",
			pgName:            "pg7",
			minMember:         2,
			podNames:          []string{"pod1", "pod2"},
			podPhase:          v1.PodPending,
			previousPhase:     v1alpha1.PodGroupPending,
			desiredGroupPhase: v1alpha1.PodGroupFinished,
			podNextPhase:      v1.PodSucceeded,
		},
		{
			name:               "Group should not enqueue, created too long",
			pgName:             "pg8",
			minMember:          2,
			podNames:           []string{"pod1", "pod2"},
			podPhase:           v1.PodRunning,
			previousPhase:      v1alpha1.PodGroupPending,
			desiredGroupPhase:  v1alpha1.PodGroupPending,
			podGroupCreateTime: &createTime,
		},
		{
			name:              "Group min member more than Pod number",
			pgName:            "pg9",
			minMember:         3,
			podNames:          []string{"pod91", "pod92"},
			podPhase:          v1.PodPending,
			previousPhase:     v1alpha1.PodGroupPending,
			desiredGroupPhase: v1alpha1.PodGroupPending,
		},
		{
			name:              "Group status convert from running to pending",
			pgName:            "pg10",
			minMember:         4,
			podNames:          []string{"pod101", "pod102"},
			podPhase:          v1.PodPending,
			previousPhase:     v1alpha1.PodGroupRunning,
			desiredGroupPhase: v1alpha1.PodGroupPending,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			controller, kClient := setUp(ctx, c.podNames, c.pgName, c.podPhase, c.minMember, c.previousPhase, c.podGroupCreateTime, nil)
			ps := makePods(c.podNames, c.pgName, c.podPhase, nil)
			// 0 means not set
			if len(c.podNextPhase) != 0 {
				ps = makePods(c.podNames, c.pgName, c.podNextPhase, nil)
			}
			for _, p := range ps {
				kClient.Status().Update(ctx, p)
				reqs := controller.podToPodGroup(ctx, p)
				for _, req := range reqs {
					if _, err := controller.Reconcile(ctx, req); err != nil {
						t.Errorf("reconcile: (%v)", err)
					}
				}
			}

			pg := &v1alpha1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.pgName,
					Namespace: metav1.NamespaceDefault,
				},
			}
			err := kClient.Get(ctx, client.ObjectKeyFromObject(pg), pg)
			if err != nil {
				t.Fatal(err)
			}

			if pg.Status.Phase != c.desiredGroupPhase {
				t.Fatalf("want %v, got %v", c.desiredGroupPhase, pg.Status.Phase)
			}
			if err != nil {
				t.Fatal("Unexpected error", err)
			}
		})
	}
}

func TestFillGroupStatusOccupied(t *testing.T) {
	ctx := context.TODO()
	cases := []struct {
		name                 string
		pgName               string
		minMember            int32
		podNames             []string
		podPhase             v1.PodPhase
		podOwnerReference    []metav1.OwnerReference
		groupPhase           v1alpha1.PodGroupPhase
		desiredGroupOccupied []string
	}{
		{
			name:      "fill the Occupied of PodGroup with a single ownerReference",
			pgName:    "pg",
			minMember: 2,
			podNames:  []string{"pod1", "pod2"},
			podPhase:  v1.PodPending,
			podOwnerReference: []metav1.OwnerReference{
				{
					Name: "new-occupied",
				},
			},
			groupPhase:           v1alpha1.PodGroupPending,
			desiredGroupOccupied: []string{"default/new-occupied"},
		},
		{
			name:      "fill the Occupied of PodGroup with multi ownerReferences",
			pgName:    "pg",
			minMember: 2,
			podNames:  []string{"pod1", "pod2"},
			podPhase:  v1.PodPending,
			podOwnerReference: []metav1.OwnerReference{
				{
					Name: "new-occupied-1",
				},
				{
					Name: "new-occupied-2",
				},
			},
			groupPhase:           v1alpha1.PodGroupPending,
			desiredGroupOccupied: []string{"default/new-occupied-1", "default/new-occupied-2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			controller, kClient := setUp(ctx, c.podNames, c.pgName, c.podPhase, c.minMember, c.groupPhase, nil, c.podOwnerReference)
			controller.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      c.pgName,
					Namespace: metav1.NamespaceDefault,
				},
			})

			pg := &v1alpha1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.pgName,
					Namespace: metav1.NamespaceDefault,
				},
			}
			err := kClient.Get(ctx, client.ObjectKeyFromObject(pg), pg)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(c.desiredGroupOccupied)
			desiredGroupOccupied := strings.Join(c.desiredGroupOccupied, ",")
			if pg.Status.OccupiedBy != desiredGroupOccupied {
				t.Fatalf("want %v, got %v", desiredGroupOccupied, pg.Status.OccupiedBy)
			}
		})
	}
}

func setUp(ctx context.Context,
	podNames []string,
	pgName string,
	podPhase v1.PodPhase,
	minMember int32,
	groupPhase v1alpha1.PodGroupPhase,
	podGroupCreateTime *metav1.Time,
	podOwnerReference []metav1.OwnerReference) (*PodGroupReconciler, client.WithWatch) {
	s := scheme.Scheme
	pg := makePG(pgName, minMember, groupPhase, podGroupCreateTime)
	s.AddKnownTypes(v1alpha1.SchemeGroupVersion, pg)
	objs := []runtime.Object{pg}
	if len(podNames) != 0 {
		ps := makePods(podNames, pgName, podPhase, podOwnerReference)
		objs = append(objs, ps[0], ps[1])
	}
	client := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.PodGroup{}).
		WithRuntimeObjects(objs...).
		Build()

	controller := &PodGroupReconciler{
		Client:   client,
		Scheme:   s,
		recorder: record.NewFakeRecorder(3),

		log: klogr.New().WithName("podGroupTest"),
	}

	return controller, client
}

func makePods(podNames []string, pgName string, phase v1.PodPhase, reference []metav1.OwnerReference) []*v1.Pod {
	pds := make([]*v1.Pod, 0)
	for _, name := range podNames {
		pod := st.MakePod().Namespace("default").Name(name).Obj()
		pod.Labels = map[string]string{v1alpha1.PodGroupLabel: pgName}
		pod.Status.Phase = phase
		if len(reference) != 0 {
			pod.OwnerReferences = reference
		}
		pds = append(pds, pod)
	}
	return pds
}

func makePG(pgName string, minMember int32, previousPhase v1alpha1.PodGroupPhase, createTime *metav1.Time) *v1alpha1.PodGroup {
	pg := &v1alpha1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              pgName,
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: v1alpha1.PodGroupSpec{
			MinMember: minMember,
		},
		Status: v1alpha1.PodGroupStatus{
			OccupiedBy:        "test",
			ScheduleStartTime: metav1.Time{Time: time.Now()},
			Phase:             previousPhase,
		},
	}
	if createTime != nil {
		pg.CreationTimestamp = *createTime
	}
	return pg
}
