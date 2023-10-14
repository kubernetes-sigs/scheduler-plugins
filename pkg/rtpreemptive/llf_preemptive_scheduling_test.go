package rtpreemptive

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/annotations"
	"sigs.k8s.io/scheduler-plugins/pkg/rtpreemptive/deadline"
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
	lowLaxity, highLaxity := map[string]string{annotations.AnnotationKeyDDL: "10s", annotations.AnnotationKeyExecTime: "9s"}, map[string]string{annotations.AnnotationKeyDDL: "20s", annotations.AnnotationKeyExecTime: "10s"}
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
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(lowLaxity).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[1]).Annotations(highLaxity).Obj()),
			},
			expected: true,
		},
		{
			name: "p1.prio == p2 prio, p1 ddl later than p2 ddl, p2 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(highLaxity).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[1]).Annotations(lowLaxity).Obj()),
			},
			expected: false,
		},
		{
			name: "p1.prio == p2 prio, equal creation time and ddl, sort by name string, p1 scheduled first",
			p1: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns1).UID("pod1").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(lowLaxity).Obj()),
			},
			p2: &framework.QueuedPodInfo{
				PodInfo: testutil.MustNewPodInfo(t, st.MakePod().Namespace(ns2).UID("pod2").Priority(lowPriority).CreationTimestamp(times[0]).Annotations(lowLaxity).Obj()),
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
