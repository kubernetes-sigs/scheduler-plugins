package preemption

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/util"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestResumePausedPod(t *testing.T) {
	res := map[v1.ResourceName]string{v1.ResourceMemory: "150", v1.ResourceCPU: "5"}
	for _, tt := range []struct {
		name                    string
		pod                     *v1.Pod
		existPods               []*v1.Pod
		nodes                   []*v1.Node
		expectedCandidateNil    bool
		expectedCandidateNode   string
		expectedCandidatePodUID types.UID
	}{
		{
			name: "found candidate pod to resume",
			pod:  util.MakePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				util.MakePod("t1-p2", "ns1", 50, 1, "5s", "t1-p2", "node-a", nil, v1.PodPaused),
				util.MakePod("t1-p3", "ns2", 50, 2, "20s", "t1-p3", "node-a", nil, v1.PodRunning),
				util.MakePod("t1-p4", "ns2", 50, 2, "30s", "t1-p4", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			expectedCandidateNode:   "node-a",
			expectedCandidatePodUID: "t1-p2",
		},
		{
			name: "found mutilple candidate pod to resume",
			pod:  util.MakePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				util.MakePod("t1-p2", "ns1", 50, 1, "10s", "t1-p2", "node-a", nil, v1.PodPaused),
				util.MakePod("t1-p3", "ns2", 50, 2, "8s", "t1-p3", "node-b", nil, v1.PodPaused),
				util.MakePod("t1-p4", "ns2", 50, 2, "6s", "t1-p4", "node-a", nil, v1.PodPaused),
				util.MakePod("t1-p5", "ns2", 50, 2, "7s", "t1-p5", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
				st.MakeNode().Name("node-b").Capacity(res).Obj(),
			},
			expectedCandidateNode:   "node-a",
			expectedCandidatePodUID: "t1-p4",
		},
		{
			name: "not enought resource to resume pod",
			pod:  util.MakePod("t1-p1", "ns1", 50, 1, "10s", "t1-p1", "", nil, v1.PodPending),
			existPods: []*v1.Pod{
				util.MakePod("t1-p2", "ns1", 50, 1, "5s", "t1-p2", "node-a", nil, v1.PodPaused),
				util.MakePod("t1-p3", "ns2", 50, 1, "6s", "t1-p3", "node-a", nil, v1.PodRunning),
				util.MakePod("t1-p4", "ns2", 50, 2, "7s", "t1-p4", "node-a", nil, v1.PodRunning),
				util.MakePod("t1-p5", "ns2", 50, 2, "7s", "t1-p5", "node-a", nil, v1.PodRunning),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node-a").Capacity(res).Obj(),
			},
			expectedCandidateNil: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			nodeLister := informerFactory.Core().V1().Nodes().Lister()
			podLister := informerFactory.Core().V1().Pods().Lister()
			sharedLister := testutil.NewFakeSharedLister(tt.existPods, tt.nodes)
			nodeInfoLister := sharedLister.NodeInfos()
			manager := NewPreemptionManager(podLister, nodeLister, nodeInfoLister, cs)
			for _, existingPod := range tt.existPods {
				if existingPod.Status.Phase == v1.PodPaused {
					manager.AddPausedPod(&Candidate{NodeName: existingPod.Spec.NodeName, Pod: existingPod})
				}
			}

			actualCondidate := manager.ResumePausedPod(context.TODO(), tt.pod)
			if tt.expectedCandidateNil {
				assert.Nil(t, actualCondidate)
			} else {
				assert.Equal(t, tt.expectedCandidateNode, actualCondidate.NodeName)
				assert.Equal(t, tt.expectedCandidatePodUID, actualCondidate.Pod.UID)
			}
		})
	}
}
