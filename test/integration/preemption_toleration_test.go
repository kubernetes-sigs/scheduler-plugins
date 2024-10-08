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
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	schedconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/preemptiontoleration"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestPreemptionTolerationPlugin(t *testing.T) {
	testCtx := &testContext{}

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	testPriorityClassName := "priority-class-for-victim-candidates"
	testPriority := int32(1000)
	pauseImage := imageutils.GetPauseImageName()
	podRequest := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("2"),
		v1.ResourceMemory: resource.MustParse("1Gi"),
	}
	// nodeCapacity < 2 * podRequest
	nodeCapacity := map[v1.ResourceName]string{
		v1.ResourceCPU:    "3",
		v1.ResourceMemory: "3Gi",
	}
	node := st.MakeNode().Name("node-a").Capacity(nodeCapacity).Obj()

	tests := []struct {
		name          string
		priorityClass *schedulingv1.PriorityClass
		preemptor     *v1.Pod
		canTolerate   bool
		// victim's scheduled time will be updated at time.Now()-<the duration>
		// this will be used to simulate preemption toleration seconds has been elapsed
		simulateVictimScheduledBefore time.Duration
	}{
		{
			name: "when preemptor's priority >= MinimumPreemptablePriority(=testPriority+10), it can NOT tolerate the preemption",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-1", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 10),
			).ResourceRequests(podRequest).Obj(),
			canTolerate: false,
		},
		{
			name: "when preemptor's priority >= MinimumPreemptablePriority(=testPriority+10), TolerationSeconds has no effect (it can NOT tolerate the preemption)",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-2", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
				preemptiontoleration.AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 30),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 10),
			).ResourceRequests(podRequest).Obj(),
			canTolerate: false,
		},
		{
			name: "when preemptor's priority < MinimumPreemptablePriority(=testPriority+10), it can not tolerate at all if no TolerationSeconds (defaut is 0)",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-3", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 9),
			).ResourceRequests(podRequest).Obj(),
			canTolerate: false,
		},
		{
			name: "when preemptor's priority < MinimumPreemptablePriority(=testPriority+10), it can tolerate forever if tolerationSeconds = -1",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-4", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
				preemptiontoleration.AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", -1),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 9),
			).ResourceRequests(podRequest).Obj(),
			simulateVictimScheduledBefore: time.Since(time.Time{}), // simualte forever
			canTolerate:                   true,
		},
		{
			name: "when preemptor's priority < MinimumPreemptablePriority(=testPriority+10), it can tolerate the preemption if victimCandidate is scheduled within TolerationSeconds",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-5", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
				preemptiontoleration.AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 30),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 5),
			).ResourceRequests(podRequest).Obj(),
			canTolerate: true,
		},
		{
			name: "when preemptor's priority < MinimumPreemptablePriority(=testPriority+10), it can NOT tolerate the preemption after TolerationSeconds elapsed from victimCandidate being scheduled",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-6", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: fmt.Sprintf("%d", testPriority+10),
				preemptiontoleration.AnnotationKeyTolerationSeconds:          fmt.Sprintf("%d", 5),
			}),
			preemptor: makePod(
				st.MakePod().Name("p").Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 5),
			).ResourceRequests(podRequest).Obj(),
			simulateVictimScheduledBefore: 10 * time.Second,
			canTolerate:                   false,
		},
		{
			name: "when unparsable preemption toleration policy, victim should be preempted if preemptor's preemption policy is PreemptLowerPriority",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-7", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: "a",
			}),
			preemptor: makePod(
				st.MakePod().Name("p").PreemptionPolicy(v1.PreemptLowerPriority).Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 1),
			).ResourceRequests(podRequest).Obj(),
			canTolerate: false,
		},
		{
			name: "when unparsable preemption toleration policy, victim should not be preempted if preemptor's preemption policy is PreemptNone",
			priorityClass: makeTestPriorityClass(fmt.Sprintf("%s-8", testPriorityClassName), testPriority, map[string]string{
				preemptiontoleration.AnnotationKeyMinimumPreemptablePriority: "a",
			}),
			preemptor: makePod(
				st.MakePod().Name("p").PreemptionPolicy(v1.PreemptNever).Container(pauseImage).ZeroTerminationGracePeriod().Priority(testPriority + 1),
			).ResourceRequests(podRequest).Obj(),
			simulateVictimScheduledBefore: time.Since(time.Time{}), // simualte forever
			canTolerate:                   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

			// prepare test cluster
			registry := fwkruntime.Registry{preemptiontoleration.Name: preemptiontoleration.New}
			cfg, err := util.NewDefaultSchedulerComponentConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Profiles[0].Plugins.PostFilter = schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: preemptiontoleration.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			}
			cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
				Name: preemptiontoleration.Name,
				Args: &schedconfig.PreemptionTolerationArgs{
					MinCandidateNodesPercentage: 10,
					MinCandidateNodesAbsolute:   100,
				},
			})

			ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
			createNamespace(t, testCtx, ns)

			testCtx = initTestSchedulerWithOptions(
				t,
				testCtx,
				scheduler.WithProfiles(cfg.Profiles[0]),
				scheduler.WithFrameworkOutOfTreeRegistry(registry),
				scheduler.WithPodInitialBackoffSeconds(int64(0)),
				scheduler.WithPodMaxBackoffSeconds(int64(0)),
			)
			syncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			defer cleanupTest(t, testCtx)

			// Create test node
			if _, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create node: %v", err)
			}

			// create test priorityclass for evaluating PreemptionToleration policy
			if _, err := cs.SchedulingV1().PriorityClasses().Create(testCtx.Ctx, tt.priorityClass, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create priorityclass %s: %v", tt.priorityClass.Name, err)
			}

			// Create victim candidate pod and it should be scheduled
			victimCandidate := makePod(
				st.MakePod().Name("victim-candidate").Priority(testPriority).Container(pauseImage).ZeroTerminationGracePeriod(),
			).PriorityClassName(tt.priorityClass.Name).ResourceRequests(podRequest).Obj()
			if victimCandidate, err = cs.CoreV1().Pods(ns).Create(testCtx.Ctx, victimCandidate, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create victim candidate pod %q: %v", victimCandidate.Name, err)
			}
			if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 60*time.Second, false, func(ctx context.Context) (bool, error) {
				return podScheduled(cs, ns, victimCandidate.Name), nil
			}); err != nil {
				t.Fatalf("victim candidate pod %q failed to be scheduled: %v", victimCandidate.Name, err)
			}
			// simulate pod scheduled time
			if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 60*time.Second, false, func(ctx context.Context) (bool, error) {
				vc, err := cs.CoreV1().Pods(ns).Get(ctx, victimCandidate.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				vc.Status.Conditions = []v1.PodCondition{{
					Type:               v1.PodScheduled,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: time.Now().Add(-1 * tt.simulateVictimScheduledBefore)},
				}}
				_, err = cs.CoreV1().Pods(ns).UpdateStatus(testCtx.Ctx, vc, metav1.UpdateOptions{})
				if err != nil {
					return false, err
				}
				return true, nil
			}); err != nil {
				t.Fatalf("failed to update victim candidate pod %q scheduled time: %v", victimCandidate.Name, err)
			}

			// Create the preemptor pod.
			preemptor, err := cs.CoreV1().Pods(ns).Create(testCtx.Ctx, tt.preemptor, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create preemptor Pod %q: %v", tt.preemptor.Name, err)
			}

			defer cleanupPods(t, testCtx, []*v1.Pod{
				victimCandidate,
				preemptor,
			})

			if tt.canTolerate {
				// - the victim candidate pod keeps being scheduled, and
				// - the preemptor pod is not scheduled
				if err := consistently(1*time.Second, 15*time.Second, func() (bool, error) {
					a := podScheduled(cs, ns, victimCandidate.Name)
					b := podScheduled(cs, ns, tt.preemptor.Name)
					t.Logf("%s scheduled=%v, %s scheduled = %v", victimCandidate.Name, a, tt.preemptor.Name, b)
					return a && !b, nil
				}); err != nil {
					t.Fatalf("preemptor pod %q was scheduled: %v", tt.preemptor.Name, err)
				}
			} else {
				// - the preemptor pod got scheduled successfully
				// - the victim pod does not exist (preempted)
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 30*time.Second, false, func(ctx context.Context) (bool, error) {
					return podScheduled(cs, ns, tt.preemptor.Name) && util.PodNotExist(cs, ns, victimCandidate.Name), nil
				}); err != nil {
					t.Fatalf("preemptor pod %q failed to be scheduled: %v", tt.preemptor.Name, err)
				}
			}
		})
	}
}

func consistently(interval, duration time.Duration, condition func() (bool, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	finished := time.After(duration)
	count := 1
	for {
		select {
		case <-ticker.C:
			ok, err := condition()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("the condition has not satisfied in the duration at %d th try", count)
			}
			count += 1
		case <-finished:
			return nil
		}
	}
}

func makeTestPriorityClass(name string, value int32, annotations map[string]string) *schedulingv1.PriorityClass {
	return &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Value: value,
	}
}

type PodWrapper struct {
	st.PodWrapper
}

func makePod(pw *st.PodWrapper) *PodWrapper {
	return &PodWrapper{PodWrapper: *pw}
}

func (pw *PodWrapper) ResourceRequests(resource v1.ResourceList) *PodWrapper {
	for i := range pw.Spec.Containers {
		c := pw.Spec.Containers[i]
		c.Resources.Requests = resource
		pw.Spec.Containers[i] = c
	}
	return pw
}

func (pw *PodWrapper) PriorityClassName(name string) *PodWrapper {
	pw.Spec.PriorityClassName = name
	return pw
}
