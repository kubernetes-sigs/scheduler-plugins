package integration

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestLLFScheduling(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: rtpreemptive.NameLLF}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{rtpreemptive.NameLLF: rtpreemptive.NewLLF}),
	)
	syncInformerFactory(testCtx)
	// go testCtx.Scheduler.Run(testCtx.Ctx) // only needed if we want to test scheduling process, for queuesort plugin we should not run this
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	for _, nodeName := range []string{"fake-node-1", "fake-node-2"} {
		node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(150, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(100, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(150, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(100, resource.DecimalSI),
		}
		if _, err := testCtx.ClientSet.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	for _, ns := range []string{"ns1", "ns2", "ns3"} {
		createNamespace(t, testCtx, ns)
	}

	for _, tt := range []struct {
		name         string
		pods         []*v1.Pod
		expectedPods []string
	}{
		{
			name: "queue sort based on laxity",
			pods: []*v1.Pod{
				util.MakePodWithDeadline("t1-p4", "ns2", 50, 10, lowPriority, "t1-p4", "", "30s", "20s"),
				util.MakePodWithDeadline("t1-p5", "ns2", 50, 10, lowPriority, "t1-p5", "", "8s", "5s"),
				util.MakePodWithDeadline("t1-p6", "ns2", 50, 10, lowPriority, "t1-p6", "", "25s", "20s"),
			},
			expectedPods: []string{"t1-p5", "t1-p6", "t1-p4"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupPods(t, testCtx, tt.pods)

			t.Logf("Start to create the pods.")
			for _, pod := range tt.pods {
				t.Logf("creating pod %q", pod.Name)
				_, err = cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			// wait for pods in sched queue
			if err = wait.Poll(time.Millisecond*200, wait.ForeverTestTimeout, func() (bool, error) {
				pendingPods, _ := testCtx.Scheduler.SchedulingQueue.PendingPods()
				if len(pendingPods) == len(tt.pods) {
					return true, nil
				}
				return false, nil
			}); err != nil {
				t.Fatal(err)
			}

			for _, expected := range tt.expectedPods {
				actual := testCtx.Scheduler.NextPod().Pod.Name
				if actual != expected {
					t.Errorf("Expect Pod %q, but got %q", expected, actual)
				} else {
					t.Logf("Pod %q is popped out as expected.", actual)
				}
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}
