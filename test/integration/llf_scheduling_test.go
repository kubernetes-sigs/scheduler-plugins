package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
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

type LLFSchedulingSuite struct {
	suite.Suite
	testCtx *testContext
	cs      *kubernetes.Clientset
}

func TestLLFSchedulingSuite(t *testing.T) {
	suite.Run(t, new(LLFSchedulingSuite))
}

func (s *LLFSchedulingSuite) SetupTest() {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	s.NoError(err)
	cfg.Profiles[0].Plugins.QueueSort = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: rtpreemptive.NameLLF}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.PreFilter.Enabled = append(cfg.Profiles[0].Plugins.PreFilter.Enabled, schedapi.Plugin{Name: rtpreemptive.NameLLF})

	testCtx = initTestSchedulerWithOptions(
		s.T(),
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{rtpreemptive.NameLLF: rtpreemptive.NewLLF}),
	)
	syncInformerFactory(testCtx)
	s.T().Log("Init scheduler success")

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
			s.T().Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	for _, ns := range []string{"ns1", "ns2", "ns3"} {
		createNamespace(s.T(), testCtx, ns)
	}

	s.testCtx = testCtx
	s.cs = cs
}

func (s *LLFSchedulingSuite) TearDownTest() {
	cleanupTest(s.T(), s.testCtx)
}
func (s *LLFSchedulingSuite) TestLLFSchedulingQueueSort() {
	for _, tt := range []struct {
		name         string
		pods         []*v1.Pod
		expectedPods []string
	}{
		{
			name: "queue sort based on laxity",
			pods: []*v1.Pod{
				util.MakePodWithDeadline("t1-p1", "ns2", 50, 10, lowPriority, "t1-p1", "", "30s", "20s"),
				util.MakePodWithDeadline("t1-p2", "ns2", 50, 10, lowPriority, "t1-p2", "", "8s", "5s"),
				util.MakePodWithDeadline("t1-p3", "ns2", 50, 10, lowPriority, "t1-p3", "", "25s", "20s"),
			},
			expectedPods: []string{"t1-p2", "t1-p3", "t1-p1"},
		},
	} {
		s.T().Run(tt.name, func(t *testing.T) {
			defer cleanupPods(t, s.testCtx, tt.pods)

			t.Logf("Start to create the pods.")
			for _, pod := range tt.pods {
				t.Logf("creating pod %q", pod.Name)
				_, err := s.cs.CoreV1().Pods(pod.Namespace).Create(s.testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			// wait for pods in sched queue
			if err := wait.Poll(time.Millisecond*200, wait.ForeverTestTimeout, func() (bool, error) {
				pendingPods, _ := s.testCtx.Scheduler.SchedulingQueue.PendingPods()
				if len(pendingPods) == len(tt.pods) {
					return true, nil
				}
				return false, nil
			}); err != nil {
				t.Fatal(err)
			}

			for _, expected := range tt.expectedPods {
				actual := s.testCtx.Scheduler.NextPod().Pod.Name
				if actual != expected {
					t.Errorf("Expect Pod %q, but got %q", expected, actual)
				} else {
					t.Logf("Pod %q is popped out as expected.", actual)
				}
			}
		})
	}
}

func (s *LLFSchedulingSuite) TestLLFScheduling() {
	go s.testCtx.Scheduler.Run(s.testCtx.Ctx)

	for _, tt := range []struct {
		name         string
		pods         []*v1.Pod
		expectedPods []string
	}{
		{
			name: "successfully scheduled pod with lowest laxity",
			pods: []*v1.Pod{
				util.MakePodWithDeadline("t2-p1", "ns1", 50, 10, lowPriority, "t2-p1", "", "30s", "20s"),
				util.MakePodWithDeadline("t2-p2", "ns1", 50, 10, lowPriority, "t2-p2", "", "8s", "5s"),
				util.MakePodWithDeadline("t2-p3", "ns1", 200, 10, lowPriority, "t2-p3", "", "8s", "5s"),
			},
			expectedPods: []string{"t2-p1", "t2-p2"},
		},
	} {
		s.T().Run(tt.name, func(t *testing.T) {
			defer cleanupPods(t, s.testCtx, tt.pods)

			t.Logf("Start to create the pods.")
			for _, pod := range tt.pods {
				t.Logf("creating pod %q", pod.Name)
				_, err := s.cs.CoreV1().Pods(pod.Namespace).Create(s.testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
			}

			err := wait.Poll(1*time.Second, 10*time.Second, func() (bool, error) {
				for _, v := range tt.expectedPods {
					if !podScheduled(s.cs, "ns1", v) {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				t.Fatalf("%v Waiting expectedPods error: %v", tt.name, err.Error())
			}
		})
	}
}
