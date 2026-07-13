/*
Copyright 2026 The Kubernetes Authors.

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

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"k8s.io/utils/ptr"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	schedconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
	"sigs.k8s.io/scheduler-plugins/test/util"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/numaplacement"
)

// TestTopologyMatchPreemption verifies that the NUMA-aware scheduler correctly
// handles preemption when a high-priority pod cannot fit on any NUMA zone without
// evicting lower-priority pods.
//
// The scenario uses a single two-NUMA node where:
//   - zone-0: 4 CPU, 4Gi  (all capacity; victim is placed here)
//   - zone-1: 2 CPU, 2Gi  (not enough to fit the preemptor alone)
//
// The victim fills zone-0. The preemptor requires 4 CPU and 4Gi;
// it cannot fit on either zone without evicting the victim; and
// eviction is triggered by meeting several conditions, namely the
// preemptor pod being with higher priority than the victim pod.
//
// The test flow needs to simulate real NRT cache flushes, and it
// does that via topology-manager attribute change (ResyncScopeAll).
func TestTopologyMatchPreemption(t *testing.T) {
	const (
		preemptionTestNodeName = "fake-node-preempt-1"
		victimPodName          = "victim-pod"
		preemptorPodName       = "preemptor-pod"
		victimContainerName    = "cnt-0"

		lowPriorityClassName        = "nrt-preemption-low"
		lowPriorityValue      int32 = 100
		highPriorityClassName       = "nrt-preemption-high"
		highPriorityValue     int32 = 1000
	)

	type priorityConfig struct {
		priority          int32
		priorityClassName string
	}
	for _, tc := range []struct {
		name                   string
		tmCacheResyncPeriod    int64
		tmDiscardReservedNodes bool
		tmPreemptionMode       schedconfig.PreemptionMode
		victimPodPriority      priorityConfig
		preemptorPodPriority   priorityConfig
		enablePreFilter        bool
		withNumaPlacement      bool
		expectedEviction       bool
	}{
		{
			name:                   "Overreserve cache: succeeds_with_numaplacement_and_prefilter",
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       true,
		},
		{
			name:                   "Overreserve cache: fails_with_disabled_preemption_field",
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionDisabled,
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
		{
			name:                   "Overreserve cache: fails_without_numaplacement",
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,

			enablePreFilter:   true,
			withNumaPlacement: false,
			expectedEviction:  false,
		},
		{
			name:                   "Overreserve cache: fails_without_prefilter",
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			enablePreFilter:        false,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
		{
			name:                   "Overreserve cache: fails_with_equal_priority_preemptor",
			victimPodPriority:      priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
		{
			name:                   "Overreserve cache: fails_with_higher_priority_victim",
			victimPodPriority:      priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
		// TODO: Adjust the below when GetNUMAPlacementInfo is implemented for the rest of the caches
		{
			name:                   "Passthrough cache: fails_with_NRT_numaplacement_and_prefilter",
			tmCacheResyncPeriod:    0, // Passthrough
			tmDiscardReservedNodes: false,
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
		{
			name:                   "DiscardReserved cache: fails_with_NRT_numaplacement_and_prefilter",
			tmCacheResyncPeriod:    defaultCacheResyncPeriodSeconds,
			tmDiscardReservedNodes: true, // DiscardReserved
			tmPreemptionMode:       schedconfig.PreemptionEnabled,
			victimPodPriority:      priorityConfig{priority: lowPriorityValue, priorityClassName: lowPriorityClassName},
			preemptorPodPriority:   priorityConfig{priority: highPriorityValue, priorityClassName: highPriorityClassName},
			enablePreFilter:        true,
			withNumaPlacement:      true,
			expectedEviction:       false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			extTestCtx := makeNRTSchedTestContextWithPreemption(t, tc.tmCacheResyncPeriod, tc.tmDiscardReservedNodes, tc.tmPreemptionMode)
			testCtx := extTestCtx.tctx // shortcut
			defer func() {
				cleanupTest(t, testCtx)
			}()

			if err := waitForNRT(t, extTestCtx.cli); err != nil {
				t.Fatalf("timed out waiting for NRT CRD: %v", err)
			}
			ns := fmt.Sprintf("nrt-preempt-test-%v", string(uuid.NewUUID()))
			createNamespace(t, testCtx, ns)

			// we choose to the scope to simplify the NRT flush logic, which changing it is enough to trigger the cache resync
			initialNRT := makeFreedNRT(preemptionTestNodeName, kubeletconfig.PodTopologyManagerScope)
			if err := createNodeResourceTopologies(testCtx.Ctx, extTestCtx.extCli, []*topologyv1alpha2.NodeResourceTopology{initialNRT}); err != nil {
				t.Fatalf("failed to create initial NRT: %v", err)
			}
			defer cleanupNodeResourceTopologies(t, context.TODO(), extTestCtx.extCli, []*topologyv1alpha2.NodeResourceTopology{initialNRT})

			if err := createNodesFromNodeResourceTopologies(t, extTestCtx.cli, testCtx.Ctx, []*topologyv1alpha2.NodeResourceTopology{initialNRT}); err != nil {
				t.Fatalf("failed to create node: %v", err)
			}

			t.Logf("creating PriorityClasses")
			for _, pc := range []*schedulingv1.PriorityClass{
				{ObjectMeta: metav1.ObjectMeta{Name: lowPriorityClassName}, Value: lowPriorityValue},
				{ObjectMeta: metav1.ObjectMeta{Name: highPriorityClassName}, Value: highPriorityValue},
			} {
				if _, err := extTestCtx.cli.SchedulingV1().PriorityClasses().Create(testCtx.Ctx, pc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
					t.Fatalf("failed to create PriorityClass %q: %v", pc.Name, err)
				}
			}

			cfg := extTestCtx.cfg
			if tc.enablePreFilter {
				cfg.Profiles[0].Plugins.PreFilter.Enabled = append(
					cfg.Profiles[0].Plugins.PreFilter.Enabled,
					schedapi.Plugin{Name: noderesourcetopology.Name},
				)
			}

			t.Logf("Initializing the scheduler with options.")
			testCtx = initTestSchedulerWithOptions(
				t,
				testCtx,
				scheduler.WithProfiles(cfg.Profiles...),
				scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{noderesourcetopology.Name: noderesourcetopology.New}),
				// we set the backoff to 0 to avoid the pod being stuck in the unschedulable pods queue
				scheduler.WithPodInitialBackoffSeconds(0),
				scheduler.WithPodMaxBackoffSeconds(0),
				scheduler.WithPodMaxInUnschedulablePodsDuration(10*time.Second),
			)
			syncInformerFactory(testCtx)
			go testCtx.Scheduler.Run(testCtx.Ctx)
			t.Log("scheduler started")

			// test logic starts here
			t.Logf("Creating and waiting for the victim to be scheduled")
			targetCPU := "4"
			targetMemory := "4Gi"
			victimPod := makeGuaranteedPod(ns, victimPodName, victimContainerName, tc.victimPodPriority.priority, tc.victimPodPriority.priorityClassName, targetCPU, targetMemory)
			victimPod, err := extTestCtx.cli.CoreV1().Pods(ns).Create(testCtx.Ctx, victimPod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create victim pod: %v", err)
			}
			defer func() {
				extTestCtx.cli.CoreV1().Pods(ns).Delete(context.TODO(), victimPodName, metav1.DeleteOptions{})
			}()

			if err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 30*time.Second, false, func(ctx context.Context) (bool, error) {
				return podScheduled(t, extTestCtx.cli, ns, victimPodName), nil
			}); err != nil {
				t.Fatalf("victim pod %q failed to be scheduled: %v", victimPodName, err)
			}
			t.Logf("victim pod %q scheduled", victimPodName)

			victimPod, err = extTestCtx.cli.CoreV1().Pods(ns).Get(testCtx.Ctx, victimPodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to re-fetch victim pod: %v", err)
			}

			t.Logf("marking victim pod as running")
			victimPod = markPodRunningAndWait(testCtx.Ctx, t, extTestCtx.cli, victimPod)

			updatedNRTWithVictim := MakeNRT().Name(preemptionTestNodeName).
				Attributes(nrtBaseAttrs(kubeletconfig.ContainerTopologyManagerScope)).
				Zone(topologyv1alpha2.ResourceInfoList{
					makeResInfo(cpu, "4", "0"),
					makeResInfo(memory, "4Gi", "0"),
				}).
				Zone(topologyv1alpha2.ResourceInfoList{
					makeResInfo(cpu, "2", "2"),
					makeResInfo(memory, "2Gi", "2Gi"),
				}).
				Obj()

			if tc.withNumaPlacement {
				affinities := []numaplacement.ContainerAffinity{
					{
						ID: numaplacement.ContainerID{
							Namespace:     ns,
							PodName:       victimPodName,
							ContainerName: victimContainerName,
						},
						NUMANode: 0,
					},
				}
				applyNUMAPlacement(t, updatedNRTWithVictim, 2, affinities)
			}
			flushNRTViaConfigChange(t, extTestCtx, testCtx, updatedNRTWithVictim)

			t.Logf("creating the preemptor pod")
			preemptorPod := makeGuaranteedPod(ns, preemptorPodName, "cnt-0", tc.preemptorPodPriority.priority, tc.preemptorPodPriority.priorityClassName, targetCPU, targetMemory)
			if _, err := extTestCtx.cli.CoreV1().Pods(ns).Create(testCtx.Ctx, preemptorPod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create preemptor pod: %v", err)
			}
			defer func() {
				extTestCtx.cli.CoreV1().Pods(ns).Delete(context.TODO(), preemptorPodName, metav1.DeleteOptions{})
			}()

			if tc.expectedEviction {
				const timeout = 5 * time.Minute
				nrtFreed := false
				if err := wait.PollUntilContextTimeout(testCtx.Ctx, 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
					t.Logf("waiting for preemptor to be scheduled and victim to be evicted...")
					preemptorScheduled := podScheduled(t, extTestCtx.cli, ns, preemptorPodName)
					victimGone := testutil.PodNotExist(extTestCtx.cli, ns, victimPodName)
					t.Logf("current state: preemptor scheduled: %v, victim gone: %v, nrt freed: %v", preemptorScheduled, victimGone, nrtFreed)

					if victimGone && !preemptorScheduled && !nrtFreed {
						freedNRT := makeFreedNRT(preemptionTestNodeName, kubeletconfig.PodTopologyManagerScope)
						flushNRTViaConfigChange(t, extTestCtx, testCtx, freedNRT)
						nrtFreed = true
					}

					return preemptorScheduled && victimGone, nil
				}); err != nil {
					t.Errorf("preemptor pod %q was not scheduled within %s: %v", preemptorPodName, timeout, err)
				}
			} else {
				if err := consistently(1*time.Second, 20*time.Second, func() (bool, error) {
					scheduled := podScheduled(t, extTestCtx.cli, ns, preemptorPodName)
					if scheduled {
						t.Logf("preemptor unexpectedly scheduled")
					}
					return !scheduled, nil
				}); err != nil {
					t.Errorf("preemptor pod %q should have stayed pending but was scheduled: %v", preemptorPodName, err)
				}
			}
		})
	}
}

func makeNRTSchedTestContextWithPreemption(t *testing.T, cacheResyncPeriod int64, discardReservedNodes bool, preemptionMode schedconfig.PreemptionMode) extTestContext {
	t.Helper()

	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(topologyv1alpha2.AddToScheme(scheme))
	cs := clientset.NewForConfigOrDie(globalKubeConfig)
	extClient, err := ctrlclient.New(globalKubeConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}

	matchArgs := schedconfig.NodeResourceTopologyMatchArgs{
		ScoringStrategy:          schedconfig.ScoringStrategy{Type: schedconfig.LeastAllocated},
		CacheResyncPeriodSeconds: cacheResyncPeriod,
		DiscardReservedNodes:     discardReservedNodes,
		Cache: &schedconfig.NodeResourceTopologyCache{
			ResyncScope: ptr.To(schedconfig.CacheResyncScopeAll),
		},
		PreemptionMode: &preemptionMode,
	}

	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].Plugins.Reserve.Enabled = append(cfg.Profiles[0].Plugins.Reserve.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].Plugins.Score.Enabled = append(cfg.Profiles[0].Plugins.Score.Enabled, schedapi.Plugin{Name: noderesourcetopology.Name})
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: noderesourcetopology.Name,
		Args: &matchArgs,
	})

	return extTestContext{
		tctx:      testCtx,
		cli:       cs,
		extCli:    extClient,
		cfg:       cfg,
		matchArgs: matchArgs,
	}
}

// minStableAfter is the minimum time to wait for a pushed NRT update to settle, used as a
// floor for flushNRTViaConfigChange when the configured cache resync period is 0 (Passthrough).
const minStableAfter = 2 * time.Second

// flushNRTViaConfigChange pushes desired into the API with a topology-manager
// attribute change and waits for the ConfigChanged resync path to flush the cache.
func flushNRTViaConfigChange(
	t *testing.T,
	extTestCtx extTestContext,
	testCtx *testContext,
	desired *topologyv1alpha2.NodeResourceTopology,
) {
	t.Helper()

	var appliedAt time.Time
	// with the Passthrough cache (CacheResyncPeriodSeconds == 0) there is no resync loop to
	// wait for, but we still need a non-zero window: a zero timeout would make
	// wait.PollUntilContextTimeout fail immediately with "context deadline exceeded" below.
	stableAfter := max(extTestCtx.CacheResyncPeriodSeconds(2), minStableAfter)

	err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 4*stableAfter, true, func(ctx context.Context) (bool, error) {
		if err := updateNodeResourceTopologies(ctx, extTestCtx.extCli, []*topologyv1alpha2.NodeResourceTopology{desired}); err != nil {
			return false, err
		}

		if appliedAt.IsZero() {
			appliedAt = time.Now()
			t.Logf("NRT %q pushed for config resync, waiting for cache flush", desired.Name)
			return false, nil
		}

		if time.Since(appliedAt) < stableAfter {
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		t.Fatalf("NRT %q was not synced into cache via config change: %v", desired.Name, err)
	}
}

func makeResInfo(name, capacityAndAllocatable, available string) topologyv1alpha2.ResourceInfo {
	cap := resource.MustParse(capacityAndAllocatable)
	return topologyv1alpha2.ResourceInfo{
		Name:        name,
		Capacity:    cap,
		Allocatable: cap,
		Available:   resource.MustParse(available),
	}
}

func nrtBaseAttrs(scope string) topologyv1alpha2.AttributeList {
	return topologyv1alpha2.AttributeList{
		{Name: nodeconfig.AttributePolicy, Value: "single-numa-node"},
		{Name: nodeconfig.AttributeScope, Value: scope},
	}
}

// makeFreedNRT returns an NRT with full zone availability.
func makeFreedNRT(nodeName, scope string) *topologyv1alpha2.NodeResourceTopology {
	return MakeNRT().Name(nodeName).
		Attributes(nrtBaseAttrs(scope)).
		Zone(topologyv1alpha2.ResourceInfoList{
			makeResInfo(cpu, "4", "4"),
			makeResInfo(memory, "4Gi", "4Gi"),
		}).
		Zone(topologyv1alpha2.ResourceInfoList{
			makeResInfo(cpu, "2", "2"),
			makeResInfo(memory, "2Gi", "2Gi"),
		}).
		Obj()
}

// markPodRunningAndWait sets pod phase to Running via the API and polls until the
// status is visible, so informers used during NRT cache resync include the pod.
func markPodRunningAndWait(
	ctx context.Context,
	t *testing.T,
	cli clientset.Interface,
	pod *corev1.Pod,
) *corev1.Pod {
	t.Helper()

	pod.Status.Phase = corev1.PodRunning
	if _, err := cli.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update pod %q status to Running: %v", pod.Name, err)
	}

	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		p, err := cli.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Status.Phase == corev1.PodRunning, nil
	}); err != nil {
		t.Fatalf("pod %q did not reach Running: %v", pod.Name, err)
	}

	updated, err := cli.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to re-fetch pod %q: %v", pod.Name, err)
	}
	return updated
}

func makeGuaranteedPod(namespace, name, cntName string, priority int32, priorityClassName, cpuStr, memStr string) *corev1.Pod {
	pause := imageutils.GetPauseImageName()
	cpuQty := resource.MustParse(cpuStr)
	memQty := resource.MustParse(memStr)
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    cpuQty,
			corev1.ResourceMemory: memQty,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    cpuQty,
			corev1.ResourceMemory: memQty,
		},
	}
	var zero int64 = 0
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: corev1.PodSpec{
			SchedulerName: "default-scheduler",
			Containers: []corev1.Container{
				{Name: cntName, Image: pause, Resources: res},
			},
			Priority:                      &priority,
			PriorityClassName:             priorityClassName,
			TerminationGracePeriodSeconds: &zero,
		},
	}
}
