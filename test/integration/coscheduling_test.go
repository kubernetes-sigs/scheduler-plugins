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

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/controller-runtime/pkg/client"

	schedconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestCoschedulingPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := util.NewClientOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == scheduling.GroupName {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: coscheduling.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].Plugins.PostFilter.Enabled = append(cfg.Profiles[0].Plugins.PostFilter.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].Plugins.Permit.Enabled = append(cfg.Profiles[0].Plugins.Permit.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: coscheduling.Name,
		Args: &schedconfig.CoschedulingArgs{
			PermitWaitingTimeSeconds: 3,
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{coscheduling.Name: coscheduling.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", nodeName, err)
	}
	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name         string
		pods         []*v1.Pod
		podGroups    []*v1alpha1.PodGroup
		expectedPods []string
	}{
		{
			name: "equal priority, sequentially pg1 meet min and pg2 not meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 3, nil, nil),
				util.MakePG("pg1-2", ns, 4, nil, nil),
			},
			expectedPods: []string{"t1-p1-1", "t1-p1-2", "t1-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and pg2 not meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t2-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg2-1", ns, 3, nil, nil),
				util.MakePG("pg2-2", ns, 4, nil, nil),
			},
			expectedPods: []string{"t2-p1-1", "t2-p1-2", "t2-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 not meet min and 3 regular pods",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t3-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t3-p2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t3-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t3-p3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t3-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg3-1", ns, 4, nil, nil),
			},
			expectedPods: []string{"t3-p2", "t3-p3"},
		},
		{
			name: "different priority, sequentially pg1 meet min and pg2 meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t4-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg4-1", ns, 3, nil, nil),
				util.MakePG("pg4-2", ns, 3, nil, nil),
			},
			expectedPods: []string{"t4-p2-1", "t4-p2-2", "t4-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and pg2 meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t5-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(v1alpha1.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg5-1", ns, 3, nil, nil),
				util.MakePG("pg5-2", ns, 3, nil, nil),
			},
			expectedPods: []string{"t5-p2-1", "t5-p2-2", "t5-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and 3 regular pods",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t6-p4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg6-1", ns, 3, nil, nil),
			},
			expectedPods: []string{"t6-p2", "t6-p3", "t6-p4"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and p2 p3 not meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p3-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p3-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p3-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t7-p3-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg7-1", ns, 3, nil, nil),
				util.MakePG("pg7-2", ns, 4, nil, nil),
				util.MakePG("pg7-3", ns, 4, nil, nil),
			},
			expectedPods: []string{"t7-p1-1", "t7-p1-2", "t7-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and p2 p3 not meet min, pgs have min resources",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p3-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p3-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p3-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t8-p3-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg8-1", ns, 3, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("150")}),
				util.MakePG("pg8-2", ns, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
				util.MakePG("pg8-3", ns, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
			},
			expectedPods: []string{"t8-p1-1", "t8-p1-2", "t8-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and pg2 not meet min, pgs have min resources",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t9-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg9-1", ns, 3, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("150")}),
				util.MakePG("pg9-2", ns, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
			},
			expectedPods: []string{"t9-p1-1", "t9-p1-2", "t9-p1-3"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-coscheduling-test %v", tt.name)
			defer cleanupPodGroups(testCtx.Ctx, extClient, tt.podGroups)
			// create pod group
			if err := createPodGroups(testCtx.Ctx, extClient, tt.podGroups); err != nil {
				t.Fatal(err)
			}
			defer cleanupPods(t, testCtx, tt.pods)
			// Create Pods, we will expect them to be scheduled in a reversed order.
			for i := range tt.pods {
				klog.InfoS("Creating pod ", "podName", tt.pods[i].Name)
				if _, err := cs.CoreV1().Pods(tt.pods[i].Namespace).Create(testCtx.Ctx, tt.pods[i], metav1.CreateOptions{}); err != nil {
					t.Fatalf("Failed to create Pod %q: %v", tt.pods[i].Name, err)
				}
			}
			err = wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, ns, v) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}

func TestPodgroupBackoff(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	extClient := util.NewClientOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == scheduling.GroupName {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: coscheduling.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].Plugins.PostFilter.Enabled = append(cfg.Profiles[0].Plugins.PostFilter.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].Plugins.Permit.Enabled = append(cfg.Profiles[0].Plugins.Permit.Enabled, schedapi.Plugin{Name: coscheduling.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: coscheduling.Name,
		Args: &schedconfig.CoschedulingArgs{
			PermitWaitingTimeSeconds: 3,
			PodGroupBackoffSeconds:   1,
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{coscheduling.Name: coscheduling.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create Nodes.
	node1Name := "fake-node-1"
	node1 := st.MakeNode().Name(node1Name).Label("node", node1Name).Obj()
	node1.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(6, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node1.Status.Capacity = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(6, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node1, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node1, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", node1Name, err)
	}
	node2Name := "fake-node-2"
	node2 := st.MakeNode().Name(node2Name).Label("node", node2Name).Obj()
	node2.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node2.Status.Capacity = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node2, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", node2Name, err)
	}
	node3Name := "fake-node-3"
	node3 := st.MakeNode().Name(node3Name).Label("node", node3Name).Obj()
	node3.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node3.Status.Capacity = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node3, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node3, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", node3Name, err)
	}
	node4Name := "fake-node-4"
	node4 := st.MakeNode().Name(node4Name).Label("node", node4Name).Obj()
	node4.Status.Allocatable = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node4.Status.Capacity = v1.ResourceList{
		v1.ResourceCPU:  *resource.NewQuantity(2, resource.DecimalSI),
		v1.ResourcePods: *resource.NewQuantity(32, resource.DecimalSI),
	}
	node4, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node4, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Node %q: %v", node4Name, err)
	}
	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name         string
		pods         []*v1.Pod
		podGroups    []*v1alpha1.PodGroup
		expectedPods []string
	}{
		{
			name: "pg1 can not be scheduled and pg2 can be scheduled",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-4").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-5").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p1-6").Req(map[v1.ResourceName]string{v1.ResourceCPU: "4"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns).Name("t1-p2-1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Priority(
					midPriority).Label(v1alpha1.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns, 6, nil, nil),
				util.MakePG("pg2-1", ns, 1, nil, nil),
			},
			expectedPods: []string{"t1-p2-1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-coscheduling-test %v", tt.name)
			defer cleanupPodGroups(testCtx.Ctx, extClient, tt.podGroups)
			// create pod group
			if err := createPodGroups(testCtx.Ctx, extClient, tt.podGroups); err != nil {
				t.Fatal(err)
			}
			defer cleanupPods(t, testCtx, tt.pods)
			// Create Pods, we will expect them to be scheduled in a reversed order.
			for i := range tt.pods {
				klog.InfoS("Creating pod ", "podName", tt.pods[i].Name)
				if _, err := cs.CoreV1().Pods(tt.pods[i].Namespace).Create(testCtx.Ctx, tt.pods[i], metav1.CreateOptions{}); err != nil {
					t.Fatalf("Failed to create Pod %q: %v", tt.pods[i].Name, err)
				}
			}
			err = wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, ns, v) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}

func WithContainer(pod *v1.Pod, image string) *v1.Pod {
	pod.Spec.Containers[0].Name = "con0"
	pod.Spec.Containers[0].Image = image
	return pod
}

func createPodGroups(ctx context.Context, c client.Client, podGroups []*v1alpha1.PodGroup) error {
	for _, pg := range podGroups {
		if err := c.Create(ctx, pg); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupPodGroups(ctx context.Context, c client.Client, podGroups []*v1alpha1.PodGroup) {
	for _, pg := range podGroups {
		_ = c.Delete(ctx, pg)
	}
}
