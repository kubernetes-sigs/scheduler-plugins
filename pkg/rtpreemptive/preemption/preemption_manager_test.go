package preemption

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/suite"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/priorityqueue"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/util"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

var (
	deadlineManager = deadline.NewDeadlineManager()
	priorityFuncEDF = func(pod *v1.Pod) int64 {
		ddl := deadlineManager.GetPodDeadline(pod)
		return -ddl.Unix()
	}
	res       = map[v1.ResourceName]string{v1.ResourceMemory: "100", v1.ResourceCPU: "2"}
	pausedPod = []*v1.Pod{
		util.MakePod("p1", "ns1", 50, 1, "30s", "ns1-p1", "node-1", nil, v1.PodPaused, "true"),
		util.MakePod("p4", "ns2", 50, 1, "10s", "ns2-p4", "node-2", nil, v1.PodPaused, "true"),
		util.MakePod("p5", "ns1", 50, 1, "30s", "ns1-p5", "node-2", nil, v1.PodPaused, "true"),
	}
	existPods = append([]*v1.Pod{
		util.MakePod("p2", "ns1", 50, 2, "5s", "ns1-p2", "node-1", nil, v1.PodRunning, ""),
		util.MakePod("p3", "ns1", 50, 1, "20s", "ns1-p3", "node-2", nil, v1.PodRunning, ""),
	}, pausedPod...)
	podsToSchedule = []*v1.Pod{
		util.MakePod("p6", "ns1", 50, 1, "8s", "ns1-p6", "", nil, v1.PodPending, ""),       // pending pod to be scheduled
		util.MakePod("p7", "ns1", 50, 1, "8s", "ns1-p7", "node-1", nil, v1.PodRunning, ""), // pod to be paused
	}
	existNodes = []*v1.Node{
		st.MakeNode().Name("node-1").Capacity(res).Obj(),
		st.MakeNode().Name("node-2").Capacity(res).Obj(),
		st.MakeNode().Name("node-3").Capacity(res).Obj(),
	}
)

type PreemptionManagerTestSuite struct {
	suite.Suite
	clientSet       *clientsetfake.Clientset
	informerFactory informers.SharedInformerFactory
	sharedLister    framework.SharedLister
	manager         *preemptionManager
}

func TestPreemptionManagerTestSuite(t *testing.T) {
	suite.Run(t, new(PreemptionManagerTestSuite))
}

func (s *PreemptionManagerTestSuite) SetupSuite() {
	var podItems []v1.Pod

	for _, pod := range append(existPods, podsToSchedule...) {
		podItems = append(podItems, *pod)
	}
	cs := clientsetfake.NewSimpleClientset(&v1.PodList{Items: podItems})
	s.sharedLister = testutil.NewFakeSharedLister(existPods, existNodes)
	s.informerFactory = informers.NewSharedInformerFactory(cs, 0)
	podInformer := s.informerFactory.Core().V1().Pods().Informer()
	for _, pod := range append(existPods, podsToSchedule...) {
		podInformer.GetStore().Add(pod)
	}
	nodeInformer := s.informerFactory.Core().V1().Nodes().Informer()
	for _, node := range existNodes {
		nodeInformer.GetStore().Add(node)
	}
	s.clientSet = cs
}

func (s *PreemptionManagerTestSuite) SetupTest() {
	podLister := s.informerFactory.Core().V1().Pods().Lister()
	nodeLister := s.informerFactory.Core().V1().Nodes().Lister()
	nodeInfoLister := s.sharedLister.NodeInfos()
	s.manager = &preemptionManager{
		preemptors:      gocache.New(time.Hour*5, time.Second*5),
		pausedPods:      gocache.New(time.Hour*5, time.Second*5),
		deadlineManager: deadline.NewDeadlineManager(),
		podLister:       podLister,
		nodeLister:      nodeLister,
		nodeInfoLister:  nodeInfoLister,
		clientSet:       s.clientSet,
		priorityFunc:    priorityFuncEDF,
	}
	for _, pod := range existPods {
		c := &Candidate{NodeName: pod.Spec.NodeName, Pod: pod}
		if pod.Name == pausedPod[1].Name {
			c.Preemptor = existPods[0]
		}
		if pod.Name == pausedPod[0].Name {
			c.Preemptor = existPods[1]
		}
		if pod.Status.Phase == v1.PodPaused {
			s.manager.addCandidate(c)
		}
	}
}

func (s *PreemptionManagerTestSuite) TestAddRemoveCandidateConcurrent() {
	s.Equal(len(pausedPod), countPods(s.manager.pausedPods.Items()))
	var wg sync.WaitGroup
	for _, p := range pausedPod {
		wg.Add(1)
		go func(p *v1.Pod) {
			s.manager.removeCandidate(&Candidate{Pod: p, NodeName: p.Spec.NodeName})
			wg.Done()
		}(p)
	}
	wg.Wait()
	s.Equal(0, countPods(s.manager.pausedPods.Items()))
	for _, p := range pausedPod {
		wg.Add(1)
		go func(p *v1.Pod) {
			s.manager.addCandidate(&Candidate{Pod: p, NodeName: p.Spec.NodeName})
			wg.Done()
		}(p)
	}
	wg.Wait()
	s.Equal(len(pausedPod), countPods(s.manager.pausedPods.Items()))
}

func countPods(pausedPod map[string]gocache.Item) int {
	size := 0
	for _, v := range pausedPod {
		q := v.Object.(priorityqueue.PriorityQueue)
		for {
			item := q.PopItem()
			if item == nil {
				break
			}
			size++
		}
	}
	return size
}

func (s *PreemptionManagerTestSuite) TestIsPodMarkedPaused() {
	pod1 := util.MakePod("p1", "ns1", 0, 0, "12s", "uid1", "node-1", nil, v1.PodRunning, "true")
	pod2 := util.MakePod("p2", "ns1", 0, 0, "10s", "uid2", "node-2", nil, v1.PodRunning, "false")
	pod3 := util.MakePod("p3", "ns1", 0, 0, "9s", "uid3", "node-3", nil, v1.PodRunning, "")
	s.Equal(true, s.manager.IsPodMarkedPaused(pod1))
	s.Equal(false, s.manager.IsPodMarkedPaused(pod2))
	s.Equal(false, s.manager.IsPodMarkedPaused(pod3))
}

func (s *PreemptionManagerTestSuite) TestGetPausedCandidateOnNode() {
	for _, tt := range []struct {
		name              string
		nodeName          string
		expectedCandidate *Candidate
	}{
		{
			name:              "empty node name",
			nodeName:          "",
			expectedCandidate: nil,
		},
		{
			name:              "node not found",
			nodeName:          "node-abc",
			expectedCandidate: nil,
		},
		{
			name:              "get pod with highest priority correctly on node-2",
			nodeName:          "node-2",
			expectedCandidate: &Candidate{Pod: pausedPod[1], NodeName: "node-2", Preemptor: existPods[0]},
		},
		{
			name:              "get pod with highest priority correctly on node-1",
			nodeName:          "node-1",
			expectedCandidate: &Candidate{Pod: pausedPod[0], NodeName: "node-1", Preemptor: existPods[1]},
		},
	} {
		s.Run(tt.name, func() {
			s.SetupTest()
			s.Equal(tt.expectedCandidate, s.manager.GetPausedCandidateOnNode(context.Background(), tt.nodeName))
			s.Equal(len(pausedPod), countPods(s.manager.pausedPods.Items()))
		})
	}
}

func (s *PreemptionManagerTestSuite) TestResumePod() {
	for _, tt := range []struct {
		name              string
		candidate         *Candidate
		expectedErr       error
		expectedCandidate *Candidate
	}{
		{
			name:              "pod not bound to node",
			candidate:         &Candidate{Pod: podsToSchedule[0], NodeName: "node-1"},
			expectedErr:       ErrPodNotBoundToNode,
			expectedCandidate: nil,
		},
		{
			name:              "pod not found",
			candidate:         &Candidate{Pod: podsToSchedule[1], NodeName: "node-1"},
			expectedErr:       ErrPodNotFound,
			expectedCandidate: nil,
		},
		{
			name:              "insufficient resources to resume",
			candidate:         &Candidate{Pod: pausedPod[0], NodeName: "node-1"},
			expectedErr:       errors.New("failed to dry run resume pod: insufficient resources to resume pod on node"),
			expectedCandidate: nil,
		},
		{
			name:              "paused pod not found",
			candidate:         &Candidate{Pod: pausedPod[0], NodeName: "node-3"},
			expectedErr:       ErrPodNotFound,
			expectedCandidate: nil,
		},
		{
			name:              "paused pod resumed successfully",
			candidate:         &Candidate{Pod: pausedPod[1], NodeName: "node-2"},
			expectedErr:       nil,
			expectedCandidate: &Candidate{Pod: pausedPod[1], NodeName: "node-2"},
		},
	} {
		s.Run(tt.name, func() {
			s.SetupTest()
			c, err := s.manager.ResumeCandidate(context.Background(), tt.candidate)
			if tt.expectedErr == nil {
				s.NoError(err)
			} else {
				s.EqualError(err, tt.expectedErr.Error())
			}
			if tt.expectedCandidate == nil {
				s.Nil(c)
			} else {
				s.Equal(tt.candidate, c)
				q, _ := s.manager.pausedPods.Get(c.NodeName)
				s.Nil(q.(priorityqueue.PriorityQueue).GetItem(toCacheKey(c.Pod)))
				s.False(s.manager.IsPodMarkedPaused(c.Pod))
			}
		})
	}
}

func (s *PreemptionManagerTestSuite) TestPauseCandidate() {
	for _, tt := range []struct {
		name              string
		candidate         *Candidate
		expectedErr       error
		expectedCandidate *Candidate
	}{
		{
			name:              "pod failed to be paused as it is already paused",
			candidate:         &Candidate{Pod: pausedPod[0]},
			expectedErr:       ErrPodAlreadyPaused,
			expectedCandidate: nil,
		},
		{
			name:              "pod failed to be paused as it is pending",
			candidate:         &Candidate{Pod: podsToSchedule[0]},
			expectedErr:       ErrPodNotBoundToNode,
			expectedCandidate: nil,
		},
		{
			name:              "pod paused successfully",
			candidate:         &Candidate{Pod: podsToSchedule[1]},
			expectedErr:       nil,
			expectedCandidate: &Candidate{Pod: podsToSchedule[1]},
		},
	} {
		s.Run(tt.name, func() {
			c, err := s.manager.PauseCandidate(context.Background(), tt.candidate)
			if tt.expectedErr == nil {
				s.NoError(err)
			} else {
				s.EqualError(err, tt.expectedErr.Error())
			}
			if tt.expectedCandidate == nil {
				s.Nil(c)
			} else {
				s.Equal(tt.candidate.Pod.Spec.NodeName, c.NodeName)
				s.Equal(tt.candidate.Pod, c.Pod)
				q, _ := s.manager.pausedPods.Get(c.NodeName)
				s.NotNil(q)
				s.NotNil(q.(priorityqueue.PriorityQueue).GetItem(toCacheKey(c.Pod)))
				s.True(s.manager.IsPodMarkedPaused(c.Pod))
			}
		})
	}
}
