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
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
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
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"

	scheconfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	coschedulingutil "sigs.k8s.io/scheduler-plugins/pkg/util"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestCoschedulingPlugin(t *testing.T) {
	todo := context.TODO()
	ctx, cancelFunc := context.WithCancel(todo)
	testCtx := &testutils.TestContext{
		Ctx:      ctx,
		CancelFn: cancelFunc,
		CloseFn:  func() {},
	}
	registry := fwkruntime.Registry{coscheduling.Name: coscheduling.New}
	t.Log("create apiserver")
	_, config := util.StartApi(t, todo.Done())

	config.ContentType = "application/json"

	apiExtensionClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	kubeConfigPath := util.BuildKubeConfigFile(config)
	if len(kubeConfigPath) == 0 {
		t.Fatal("Build KubeConfigFile failed")
	}
	defer os.RemoveAll(kubeConfigPath)

	t.Log("create crd")
	if _, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, makeCRD(), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	cs := kubernetes.NewForConfigOrDie(config)
	extClient := pgclientset.NewForConfigOrDie(config)

	if err = wait.Poll(100*time.Millisecond, 3*time.Second, func() (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == scheduling.GroupName {
				return true, nil
			}
		}
		t.Log("waiting for crd api ready")
		return false, nil
	}); err != nil {
		t.Fatalf("Waiting for crd read time out: %v", err)
	}
	cfg := &scheconfig.CoschedulingArgs{
		KubeConfigPath:           kubeConfigPath,
		PermitWaitingTimeSeconds: 3,
	}

	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			QueueSort: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			},
			PreFilter: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
			PostFilter: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
			Permit: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
			PostBind: schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: coscheduling.Name},
				},
			},
		},
		PluginConfig: []schedapi.PluginConfig{
			{
				Name: coscheduling.Name,
				Args: cfg,
			},
		},
	}

	ns, err := cs.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))}}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("Failed to integration test ns: %v", err)
	}

	autoCreate := false
	t.Logf("namespaces %+v", ns.Name)
	_, err = cs.CoreV1().ServiceAccounts(ns.Name).Create(ctx, &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns.Name}, AutomountServiceAccountToken: &autoCreate}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("Failed to create ns default: %v", err)
	}

	testCtx.NS = ns
	testCtx.ClientSet = cs

	testCtx = util.InitTestSchedulerWithOptions(
		t,
		testCtx,
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)
	t.Log("init scheduler success")
	defer testutils.CleanupTest(t, testCtx)

	// Create a Node.
	nodeName := "fake-node"
	node := st.MakeNode().Name("fake-node").Label("node", nodeName).Obj()
	node.Status.Allocatable = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node.Status.Capacity = v1.ResourceList{
		v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(300, resource.DecimalSI),
	}
	node, err = cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
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
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t1-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg1-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg1-1", ns.Name, 3, nil, nil),
				util.MakePG("pg1-2", ns.Name, 4, nil, nil),
			},
			expectedPods: []string{"t1-p1-1", "t1-p1-2", "t1-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and pg2 not meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t2-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg2-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg2-1", ns.Name, 3, nil, nil),
				util.MakePG("pg2-2", ns.Name, 4, nil, nil),
			},
			expectedPods: []string{"t2-p1-1", "t2-p1-2", "t2-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 not meet min and 3 regular pods",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t3-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t3-p2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t3-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t3-p3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t3-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg3-1").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg3-1", ns.Name, 4, nil, nil),
			},
			expectedPods: []string{"t3-p2", "t3-p3"},
		},
		{
			name: "different priority, sequentially pg1 meet min and pg2 meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg4-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t4-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg4-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg4-1", ns.Name, 3, nil, nil),
				util.MakePG("pg4-2", ns.Name, 3, nil, nil),
			},
			expectedPods: []string{"t4-p2-1", "t4-p2-2", "t4-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and pg2 meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg5-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t5-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					highPriority).Label(coschedulingutil.PodGroupLabel, "pg5-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg5-1", ns.Name, 3, nil, nil),
				util.MakePG("pg5-2", ns.Name, 3, nil, nil),
			},
			expectedPods: []string{"t5-p2-1", "t5-p2-2", "t5-p2-3"},
		},
		{
			name: "different priority, not sequentially pg1 meet min and 3 regular pods",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg6-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t6-p4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					highPriority).ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg6-1", ns.Name, 3, nil, nil),
			},
			expectedPods: []string{"t6-p2", "t6-p3", "t6-p4"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and p2 p3 not meet min",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p3-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p3-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p3-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t7-p3-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg7-3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg7-1", ns.Name, 3, nil, nil),
				util.MakePG("pg7-2", ns.Name, 4, nil, nil),
				util.MakePG("pg7-3", ns.Name, 4, nil, nil),
			},
			expectedPods: []string{"t7-p1-1", "t7-p1-2", "t7-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and p2 p3 not meet min, pgs have min resources",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p3-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p3-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p3-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t8-p3-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg8-3").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg8-1", ns.Name, 3, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("150")}),
				util.MakePG("pg8-2", ns.Name, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
				util.MakePG("pg8-3", ns.Name, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
			},
			expectedPods: []string{"t8-p1-1", "t8-p1-2", "t8-p1-3"},
		},
		{
			name: "equal priority, not sequentially pg1 meet min and pg2 not meet min, pgs have min resources",
			pods: []*v1.Pod{
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p1-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p2-1").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p1-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p2-2").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p1-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "50"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-1").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p2-3").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
				WithContainer(st.MakePod().Namespace(ns.Name).Name("t9-p2-4").Req(map[v1.ResourceName]string{v1.ResourceMemory: "100"}).Priority(
					midPriority).Label(coschedulingutil.PodGroupLabel, "pg9-2").ZeroTerminationGracePeriod().Obj(), pause),
			},
			podGroups: []*v1alpha1.PodGroup{
				util.MakePG("pg9-1", ns.Name, 3, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("150")}),
				util.MakePG("pg9-2", ns.Name, 4, nil, &v1.ResourceList{v1.ResourceMemory: resource.MustParse("400")}),
			},
			expectedPods: []string{"t9-p1-1", "t9-p1-2", "t9-p1-3"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Start-coscheduling-test %v", tt.name)
			defer cleanupPodGroups(ctx, extClient, tt.podGroups)
			// create pod group
			if err := createPodGroups(ctx, extClient, tt.podGroups); err != nil {
				t.Fatal(err)
			}
			defer testutils.CleanupPods(cs, t, tt.pods)
			// Create Pods, We will expect them to be scheduled in a reversed order.
			for i := range tt.pods {
				klog.InfoS("Creating pod ", "podName", tt.pods[i].Name)
				_, err := cs.CoreV1().Pods(tt.pods[i].Namespace).Create(testCtx.Ctx, tt.pods[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", tt.pods[i].Name, err)
				}

			}
			err = wait.Poll(1*time.Second, 120*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(cs, ns.Name, v) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
			t.Logf("case %v finished", tt.name)
		})
	}
}

func makeCRD() *apiextensionsv1.CustomResourceDefinition {
	var min = 1.0
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "podgroups.scheduling.sigs.k8s.io",
			Annotations: map[string]string{
				"api-approved.kubernetes.io": "https://github.com/kubernetes-sigs/scheduler-plugins/pull/52",
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: scheduling.GroupName,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"minMember": {
										Type:    "integer",
										Minimum: &min,
									},
									"minResources": {
										Type: "object",
										AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
											Schema: &apiextensionsv1.JSONSchemaProps{
												Type: "string",
											},
										},
									},
								},
							},
						},
					},
				}}},
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:       "PodGroup",
				Plural:     "podgroups",
				ShortNames: []string{"pg", "pgs"},
			},
		},
	}
}

func WithContainer(pod *v1.Pod, image string) *v1.Pod {
	pod.Spec.Containers[0].Name = "con0"
	pod.Spec.Containers[0].Image = image
	return pod
}

func createPodGroups(ctx context.Context, client pgclientset.Interface, podGroups []*v1alpha1.PodGroup) error {
	for _, pg := range podGroups {
		_, err := client.SchedulingV1alpha1().PodGroups(pg.Namespace).Create(ctx, pg, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func cleanupPodGroups(ctx context.Context, client pgclientset.Interface, podGroups []*v1alpha1.PodGroup) {
	for _, pg := range podGroups {
		client.SchedulingV1alpha1().PodGroups(pg.Namespace).Delete(ctx, pg.Name, metav1.DeleteOptions{})
	}
}
