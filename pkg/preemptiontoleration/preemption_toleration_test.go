/*
Copyright 2021 The Kubernetes Authors.

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

package preemptiontoleration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

var (
	testPriorityClassName = "dummy"
)

type testCase struct {
	name                         string
	victimCandidatePriorityClass *schedulingv1.PriorityClass
	victimCandidate              *corev1.Pod
	preemptor                    *corev1.Pod
	now                          time.Time
	wantErr                      bool
	want                         bool
	errSubStr                    string
}

func TestExemptedFromPreemptionWithNonExitingPriorityClass(t *testing.T) {
	for _, tt := range []testCase{
		{
			name:                         "when priority class does not exist, it should return error",
			victimCandidatePriorityClass: nil,
			victimCandidate:              makePod().PriorityClassName(testPriorityClassName).Priority(3).Obj(),
			preemptor:                    makePod().PreemptionPolicy(corev1.PreemptLowerPriority).Priority(2).Obj(),
			wantErr:                      true,
			errSubStr:                    fmt.Sprintf(`priorityclass.scheduling.k8s.io "%s" not found`, testPriorityClassName),
		},
	} {
		t.Run(tt.name, tt.run)
	}
}

func TestExemptedFromPreemptionWithUnparsalbePolicy(t *testing.T) {
	for _, tt := range []testCase{
		{
			name: "when MinimumPreemptablePriority not parsable, it should return false",
			victimCandidatePriorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: "a",
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).Priority(3).Obj(),
			preemptor:       makePod().PreemptionPolicy(corev1.PreemptLowerPriority).Priority(2).Obj(),
			want:            false,
		},
		{
			name: "when MinimumPreemptablePriority not parsable, it should return false",
			victimCandidatePriorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: "10",
				AnnotationKeyTolerationSeconds:          "a",
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).Priority(3).Obj(),
			preemptor:       makePod().PreemptionPolicy(corev1.PreemptLowerPriority).Priority(2).Obj(),
			want:            false,
		},
	} {
		t.Run(tt.name, tt.run)
	}
}

func TestExemptedFromPreemptionWithoutPolicy(t *testing.T) {
	for _, tt := range []testCase{
		{
			name:                         "when preemptor's PreemptionPolicy is PreemptNever, it should return true",
			victimCandidatePriorityClass: makePriorityClass(1, nil),
			victimCandidate:              makePod().PriorityClassName(testPriorityClassName).Priority(1).Obj(),
			preemptor:                    makePod().PreemptionPolicy(corev1.PreemptNever).Priority(2).Obj(),
			want:                         true,
		},
		{
			name:                         "when preemptor's PreemptionPolicy is PreemptLowerPriority, it should return false when victimCandidate's priority is lower than preemptor's one",
			victimCandidatePriorityClass: makePriorityClass(1, nil),
			victimCandidate:              makePod().PriorityClassName(testPriorityClassName).Priority(1).Obj(),
			preemptor:                    makePod().PreemptionPolicy(corev1.PreemptLowerPriority).Priority(2).Obj(),
			want:                         false,
		},
		{
			name:                         "when preemptor's PreemptionPolicy is PreemptLowerPriority, it should return true when victimCandidate's priority is not lower than preemptor's one",
			victimCandidatePriorityClass: makePriorityClass(3, nil),
			victimCandidate:              makePod().PriorityClassName(testPriorityClassName).Priority(3).Obj(),
			preemptor:                    makePod().PreemptionPolicy(corev1.PreemptLowerPriority).Priority(2).Obj(),
			want:                         true,
		},
	} {
		t.Run(tt.name, tt.run)
	}
}

func TestExemptedFromPreemptionWithPolicyWhenPreemptorPriorityHigherOrEqualMinimumPreemptablePriority(t *testing.T) {
	now := time.Now()
	victimCandidatePriority := int32(100)
	minimumPreemptablePriority := int32(200)
	for _, tt := range []testCase{
		// When preemptor's priority >= MinimumPreemptablePriorityClass, no toleration at all
		{
			name: "when preemptor's priority is higher than MinimumPreemptablePriorityClass, it should return false",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(now).Priority(victimCandidatePriority).Obj(),
			preemptor:       makePod().Priority(minimumPreemptablePriority + 1).Obj(),
			now:             now,
			want:            false,
		},
		{
			name: "when preemptor's priority is equal to MinimumPreemptablePriorityClass, it should return false",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(now).Priority(victimCandidatePriority).Obj(),
			preemptor:       makePod().Priority(minimumPreemptablePriority).Obj(),
			now:             now,
			want:            false,
		},
	} {
		t.Run(tt.name, tt.run)
	}
}

func TestExemptedFromPreemptionWithPolicyWhenPreemptorPriorityLowerThanMinimumPreemptablePriority(t *testing.T) {
	now := time.Now()
	victimCandidatePriority := int32(100)
	minimumPreemptablePriority := int32(200)
	preemptor := makePod().Priority(minimumPreemptablePriority - 1).Obj()
	for _, tt := range []testCase{
		{
			name: "when TolerationSeconds is 0, it should return false (no toleration)",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
				AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 0),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(now).Priority(victimCandidatePriority).Obj(),
			preemptor:       preemptor,
			now:             now,
			want:            false,
		},
		{
			name: "when TolerationSeconds is negative, it should return true (can tolerate forever)",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
				AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", -1),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(time.Time{}).Priority(victimCandidatePriority).Obj(), // scheduled very long time ago
			preemptor:       preemptor,
			now:             now,
			want:            true,
		},
		{
			name: "when TolerationSeconds is positive and victimCandidate has elapsed TolerationSeconds from being scheduled, it should return false (the toleratation expired)",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
				AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 100),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(now.Add(-101 * time.Second)).Priority(victimCandidatePriority).Obj(),
			preemptor:       preemptor,
			now:             now,
			want:            false,
		},
		{
			name: "when TolerationSeconds is positive and victimCandidate has been scheduled within TolerationSeconds, it should return true (the toleration hasn't expired)",
			victimCandidatePriorityClass: makePriorityClass(victimCandidatePriority, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", minimumPreemptablePriority),
				AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 100),
			}),
			victimCandidate: makePod().PriorityClassName(testPriorityClassName).ScheduledAt(now.Add(-10 * time.Second)).Priority(victimCandidatePriority).Obj(),
			preemptor:       preemptor,
			now:             now,
			want:            true,
		},
	} {
		t.Run(tt.name, tt.run)
	}
}

func (tt testCase) run(t *testing.T) {
	t.Helper()
	var fakeClient *fake.Clientset
	if tt.victimCandidatePriorityClass != nil {
		// fakeClient requires TypeMeta for objects
		tt.victimCandidatePriorityClass.TypeMeta = metav1.TypeMeta{
			APIVersion: schedulingv1.SchemeGroupVersion.String(),
			Kind:       "PriorityClass",
		}
		fakeClient = fake.NewSimpleClientset(tt.victimCandidatePriorityClass)
	} else {
		fakeClient = fake.NewSimpleClientset()
	}
	informersFactory := informers.NewSharedInformerFactory(fakeClient, 1*time.Minute)
	pcInformer := informersFactory.Scheduling().V1().PriorityClasses().Informer()
	informersFactory.Start(context.Background().Done())
	cache.WaitForCacheSync(context.Background().Done(), pcInformer.HasSynced)

	now := tt.now
	if tt.now.IsZero() {
		now = time.Now()
	}
	got, err := ExemptedFromPreemption(
		tt.victimCandidate, tt.preemptor,
		informersFactory.Scheduling().V1().PriorityClasses().Lister(),
		now,
	)

	if tt.wantErr {
		if err == nil {
			t.Errorf("expected error")
			return
		}
		if !strings.Contains(err.Error(), tt.errSubStr) {
			t.Errorf("Unexpected error message wantSubString: %s, got: %s", tt.errSubStr, err.Error())
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if tt.want != got {
			t.Errorf("Unexpected result want: %v, got: %v", tt.want, got)
		}
	}
}

type PodWrapper struct {
	st.PodWrapper
}

func makePod() *PodWrapper {
	return &PodWrapper{PodWrapper: *st.MakePod()}
}

func (pw *PodWrapper) PriorityClassName(name string) *PodWrapper {
	pw.Spec.PriorityClassName = name
	return pw
}

func (pw *PodWrapper) ScheduledAt(at time.Time) *PodWrapper {
	pw.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: at},
	}}
	return pw
}

func makePriorityClass(value int32, annotations map[string]string) *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: testPriorityClassName, Annotations: annotations},
		Value:      value,
	}
}
