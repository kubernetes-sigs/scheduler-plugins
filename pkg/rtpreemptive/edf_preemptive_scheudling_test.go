package rtpreemptive

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	plfeature "k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/preemption"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestLess(t *testing.T) {
	now := time.Now()
	times := make([]metav1.Time, 0)
	for _, d := range []time.Duration{0, 1, 2} {
		times = append(times, metav1.Time{Time: now.Add(d * time.Second)})
	}
	ns1, ns2 := "namespace1", "namespace2"
	lowPriority, highPriority := int32(10), int32(100)
	shortDDL, longDDL := map[string]string{deadline.AnnotationKeyDDL: "10s"}, map[string]string{deadline.AnnotationKeyDDL: "20s"}
	for _, tt := range []struct {
		name     string
		p1       *framework.QueuedPodInfo
		p2       *framework.QueuedPodInfo
		expected bool
	}{
		{
			name: "p1.prio < p2 prio, p2 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(highPriority).Obj()),
			},
			expected: false,
		},
		{
			name: "p1.prio > p2 prio, p1 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(highPriority).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).Obj()),
			},
			expected: true,
		},
		{
			name: "p1.prio == p2 prio, p1 ddl earlier than p2 ddl, p1 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(shortDDL).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[1]).Annotations(longDDL).Obj()),
			},
			expected: true,
		},
		{
			name: "p1.prio == p2 prio, p1 ddl later than p2 ddl, p2 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(longDDL).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[1]).Annotations(shortDDL).Obj()),
			},
			expected: false,
		},
		{
			name: "p1.prio == p2 prio, no ddl defined, p1 created earlier than p2, p1 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[1]).Obj()),
			},
			expected: true,
		},
		{
			name: "p1.prio == p2 prio, no ddl defined, p1 created later than p2, p2 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[1]).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[0]).Obj()),
			},
			expected: false,
		},
		{
			name: "p1.prio == p2 prio, equal creation time and ddl, sort by name string, p1 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[0]).Obj()),
			},
			expected: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			edfPreemptiveScheduling := &EDFPreemptiveScheduling{
				deadlineManager: deadline.NewDeadlineManager(),
			}
			if got := edfPreemptiveScheduling.Less(tt.p1, tt.p2); got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestPreFiter(t *testing.T) {
	for _, tt := range []struct {
		name               string
		pod                *v1.Pod
		expectedStatusCode framework.Code
	}{
		{
			name:               "pod marked to be paused - unschedulable and unresolvable",
			pod:                st.MakePod().UID("pod1").Annotations(map[string]string{preemption.AnnotationKeyPausePod: "true"}).Obj(),
			expectedStatusCode: framework.UnschedulableAndUnresolvable,
		},
		{
			name:               "pod marked not to be paused - success",
			pod:                st.MakePod().UID("pod2").Annotations(map[string]string{preemption.AnnotationKeyPausePod: "false"}).Obj(),
			expectedStatusCode: framework.Success,
		},
		{
			name:               "empty annotations - success",
			pod:                st.MakePod().UID("pod3").Obj(),
			expectedStatusCode: framework.Success,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			edfPreemptiveScheduling := &EDFPreemptiveScheduling{
				preemptionManager: preemption.NewPreemptionManager(nil, nil, nil),
				deadlineManager:   deadline.NewDeadlineManager(),
			}
			actualResult, actualStatus := edfPreemptiveScheduling.PreFilter(context.TODO(), nil, tt.pod)
			assert.Nil(t, actualResult)
			assert.Equal(t, actualStatus.Code(), tt.expectedStatusCode)
		})
	}
}

func TestPostFilter(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150", v1.ResourceCPU: "5"}
	now := time.Now()
	minuteAgo := now.Add(-time.Minute)
	tests := []struct {
		name                  string
		pod                   *v1.Pod
		existPods             []*v1.Pod
		nodes                 []*v1.Node
		filteredNodesStatuses framework.NodeToStatusMap
		expectedResult        *framework.PostFilterResult
		expectedStatus        *framework.Status
	}{
		{
			name: "found a paused pod with earlier deadline that should be resumed",
			pod:  makePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", &now, v1.PodPending),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 1, "1m5s", "t1-p2", "node-a", &minuteAgo, v1.PodPaused),
				makePod("t1-p3", "ns2", 50, 2, "10s", "t1-p3", "node-a", &now, v1.PodRunning),
				makePod("t1-p4", "ns2", 50, 2, "10s", "t1-p4", "node-a", &now, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			expectedResult: nil,
			expectedStatus: framework.NewStatus(framework.Unschedulable, "a paused pod is resumed instead"),
		},
		{
			name: "node is unschedulable and unresolvable",
			pod:  makePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 1, "1m5s", "t1-p2", "node-a", nil, v1.PodRunning),
				makePod("t1-p3", "ns2", 50, 2, "10s", "t1-p3", "node-a", nil, v1.PodRunning),
				makePod("t1-p4", "ns2", 50, 2, "10s", "t1-p4", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.UnschedulableAndUnresolvable),
			},
			expectedResult: nil,
			expectedStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, "no preemptible candidates found"),
		},
		{
			name: "found a pod with later deadline but it cannot yield enough resource",
			pod:  makePod("t1-p1", "ns1", 50, 4, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 1, "10s", "t1-p2", "node-a", nil, v1.PodRunning),
				makePod("t1-p3", "ns2", 50, 2, "20s", "t1-p3", "node-a", nil, v1.PodRunning),
				makePod("t1-p4", "ns2", 50, 2, "30s", "t1-p4", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			expectedResult: nil,
			expectedStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, "no preemptible candidates found"),
		},
		{
			name: "found a pod with later deadline that can yield enough resource",
			pod:  makePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				makePod("t1-p2", "ns1", 50, 1, "10s", "t1-p2", "node-a", nil, v1.PodRunning),
				makePod("t1-p3", "ns2", 50, 2, "20s", "t1-p3", "node-a", nil, v1.PodRunning),
				makePod("t1-p4", "ns2", 50, 2, "30s", "t1-p4", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node-a": framework.NewStatus(framework.Unschedulable),
			},
			expectedResult: framework.NewPostFilterResultWithNominatedNode("node-a"),
			expectedStatus: framework.NewStatus(framework.Success),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registeredPlugins := makeRegisteredPlugin()

			podItems := []v1.Pod{}
			for _, pod := range tt.existPods {
				podItems = append(podItems, *pod)
			}
			cs := clientsetfake.NewSimpleClientset(&v1.PodList{Items: podItems})
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			podInformer.GetStore().Add(tt.pod)
			for _, pod := range tt.existPods {
				podInformer.GetStore().Add(pod)
			}
			nodeInformer := informerFactory.Core().V1().Nodes().Informer()
			for _, node := range tt.nodes {
				nodeInformer.GetStore().Add(node)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fwk, err := st.NewFramework(
				registeredPlugins,
				"edf-rt-scheduler",
				ctx.Done(),
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithPodNominator(testutil.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithSnapshotSharedLister(testutil.NewFakeSharedLister(tt.existPods, tt.nodes)),
			)
			if err != nil {
				t.Fatal(err)
			}

			nodeLister := informerFactory.Core().V1().Nodes().Lister()
			podLister := informerFactory.Core().V1().Pods().Lister()
			preemptionMngr := preemption.NewPreemptionManager(podLister, nodeLister, cs)
			for _, existingPod := range tt.existPods {
				if existingPod.Status.Phase == v1.PodPaused {
					preemptionMngr.AddPausedPod(&preemption.Candidate{NodeName: existingPod.Spec.NodeName, Pod: existingPod})
				}
			}
			scheduler := &EDFPreemptiveScheduling{
				fh:                fwk,
				deadlineManager:   deadline.NewDeadlineManager(),
				preemptionManager: preemptionMngr,
				podLister:         podLister,
			}
			state := framework.NewCycleState()
			actualResult, actualStatus := scheduler.PostFilter(ctx, state, tt.pod, tt.filteredNodesStatuses)
			assert.Equal(t, tt.expectedResult, actualResult)
			assert.Equal(t, tt.expectedStatus, actualStatus)
		})
	}
}

func makeRegisteredPlugin() []st.RegisterPluginFunc {
	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterQueueSortPlugin(Name, New),
		st.RegisterPreFilterPlugin(Name, New),
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterPluginAsExtensions(noderesources.Name, func(plArgs apiruntime.Object, fh framework.Handle) (framework.Plugin, error) {
			return noderesources.NewFit(plArgs, fh, plfeature.Features{})
		}, "Filter", "PreFilter"),
	}
	return registeredPlugins
}

func makePod(podName string, namespace string, memReq int64, cpuReq int64, ddl string, uid string, nodeName string, createdAt *time.Time, phase v1.PodPhase) *v1.Pod {
	now := time.Now()
	if createdAt == nil {
		createdAt = &now
	}
	pause := imageutils.GetPauseImageName()
	pod := st.MakePod().Namespace(namespace).Name(podName).Container(pause).
		Node(nodeName).UID(uid).ZeroTerminationGracePeriod().UID(podName).
		CreationTimestamp(metav1.NewTime(*createdAt)).Phase(phase).
		Annotations(map[string]string{deadline.AnnotationKeyDDL: ddl}).Obj()
	pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceMemory: *resource.NewQuantity(memReq, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(cpuReq, resource.DecimalSI),
		},
	}
	return pod
}
