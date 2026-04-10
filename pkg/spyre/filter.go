/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre

import (
	"context"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"k8s.io/klog/v2"

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	spyreconst "github.com/ibm-aiu/spyre-operator/const"
)

func (ap *SpyrePlugin) Filter(
	ctx context.Context, cycleState *framework.CycleState,
	pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {

	klog.InfoS("starting filter process", "pod", pod, "node", nodeInfo.Node().Name)
	return ap.filter(ctx, pod, nodeInfo.Node().Name)
}

func (ap *SpyrePlugin) filter(ctx context.Context, pod *corev1.Pod, nodeName string) *framework.Status {

	klog.Info("checking number of requested spyre PFs/VFs")
	m, err := getNumRequestedDevices(pod)
	if err != nil {
		return framework.AsStatus(err)
	}

	if len(m) == 0 {
		klog.Info("skip filter operation because Pod does not request neither PF nor VF devices.")
		return framework.NewStatus(framework.Success)
	}

	klog.Info("check state in SpyreClusterPolicy")
	spyrepol, err := ap.spyreClient.GetSpyreClusterPolicy(ctx, "spyreclusterpolicy")
	if err != nil {
		return framework.AsStatus(
			fmt.Errorf("failed to get SpyreClusterPolicy: %w", err))
	}

	nodeState, err := ap.cleanupOrphanReservation(ctx, nodeName, pod)
	if err != nil {
		return framework.AsStatus(err)
	}

	// card management Pod
	if isCardManagementPod(pod, spyrepol.Status.Namespace) {
		if _, err = ap.SelectDevicesForCardManagement(pod, nodeState, nodeName); err != nil {
			return framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
		} else {
			return framework.NewStatus(framework.Success)
		}
	}

	// user Pod
	if len(m) > 1 {
		return framework.AsStatus(fmt.Errorf("User Pod cannot request both PF and VF devices."))
	}

	var numRequested int64
	var resourceName string

	for k, v := range m {
		resourceName = k
		numRequested = int64(v)
	}

	klog.Infof("Pod requests %d %s.", numRequested, resourceName)

	isCardMgmtRunner := isCardManagementRunnerPod(pod, spyrepol.Status.Namespace)

	if !isCardMgmtRunner { // user Pod

		// health-based filtering
		if spyrepol.Status.State != spyrev1alpha1.Ready {
			return framework.AsStatus(
				fmt.Errorf("SpyreClusterPolicy is not ready: %s", spyrepol.Status.State))
		}

		// cardmgmt-based filtering
		pfAvailable, msg := pfAvailableForUserPod(nodeName, spyrepol)
		if !pfAvailable && resourceName == spyreconst.PfResourceName {
			return framework.NewStatus(
				framework.UnschedulableAndUnresolvable,
				fmt.Sprintf("user Pod %s/%s cannot acquire PF device on node %s: %s",
					pod.Namespace, pod.Name, nodeName, msg))
		}

		klog.Infof("user Pod %s/%s is eligible to acquire device on node %s: %s",
			pod.Namespace, pod.Name, nodeName, msg)
	}

	if _, _, err = ap.SelectDevices(ctx, isCardMgmtRunner, resourceName, numRequested, nodeName); err != nil {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	}
	return framework.NewStatus(framework.Success)
}

// pfAvailableForUserPod returns true if user Pod (not a cardmgmt Pod) can use PF devices.
// Second return value contains information about the true/false decision.
func pfAvailableForUserPod(nodeName string, spyrepol *spyrev1alpha1.SpyreClusterPolicy) (bool, string) {

	if !spyrepol.Spec.CardManagement.Enabled {
		return true, "cardmgmt is not enabled"
	}

	spyrefilter := "." // default value of cardmgmt
	if spyrepol.Spec.CardManagement.Config != nil &&
		spyrepol.Spec.CardManagement.Config.SpyreFilter != nil {
		spyrefilter = *spyrepol.Spec.CardManagement.Config.SpyreFilter
	}
	re, err := regexp.Compile(spyrefilter)
	if err != nil {
		return true, fmt.Sprintf(
			"bypassed due to cardmgmt configuration error (spyrefilter: %s)",
			spyrefilter)
	}

	if re.MatchString(nodeName) {
		return false, fmt.Sprintf(
			"cardmgmt manages node %s (spyrefilter: %s)",
			nodeName, spyrefilter)
	} else {
		return true, fmt.Sprintf(
			"cardmgmt does not manage node %s (spyrefilter: %s)",
			nodeName, spyrefilter)
	}

}
