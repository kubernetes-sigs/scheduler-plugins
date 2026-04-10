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

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	spyrensUpdateTrialLimit = 10
)

// this file implements ReservePlugin interface of the scheduling framework.

// Reserve converts per-device-type resource requests to per-device request in the Pod, and
// then add the devices to reservedSpyreInterfaces in SpyreNodeState.
func (ap *SpyrePlugin) Reserve(
	ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) *framework.Status {
	klog.Info("start the process in Reserve extension point")

	klog.Info("trying to reserve requested devices")
	err := ap.reserveDevices(ctx, p, nodeName)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error())
	}

	return framework.NewStatus(framework.Success)
}

func (ap *SpyrePlugin) reserveDevices(ctx context.Context, p *corev1.Pod, nodeName string) error {

	klog.Info("getting number of requested devices")
	m, err := getNumRequestedDevices(p)
	if err != nil {
		return err
	}

	if len(m) == 0 {
		klog.Info("skip reserve operation because Pod does not request neither PF nor VF devices.")
		return nil
	}

	var nReq int64
	var rName string

	for k, v := range m {
		rName = k
		nReq = int64(v)
	}
	klog.Infof("Spyre requests: %d (%v)", nReq, rName)

	rDevs := map[string][]string{} // resource name => set of devices

	klog.Info("getting SpyreClusterPolicy")
	spyrepol, err := ap.spyreClient.GetSpyreClusterPolicy(ctx, "spyreclusterpolicy")
	if err != nil {
		return fmt.Errorf("failed to get AiuClusterPolicy: %w", err)
	}

	nodeState, err := ap.cleanupOrphanReservation(ctx, nodeName, p)
	if err != nil {
		return err
	}

	if isCardManagementPod(p, spyrepol.Status.Namespace) {
		klog.Info("reserving devices for Card Management Pod")
		rDevs, err = ap.SelectDevicesForCardManagement(p, nodeState, nodeName)
		if err != nil {
			return fmt.Errorf("failed to select devices for Card Management Pod: %w", err)
		}
	} else {
		if len(m) > 1 {
			return fmt.Errorf("User Pod cannot request both PF and VF devices.")
		}
		isCardMgmtRunner := isCardManagementRunnerPod(p, spyrepol.Status.Namespace)
		klog.Info("checking remaining devices")
		nRemaining, err := ap.getNumRemainingDevices(ctx, nodeState, isCardMgmtRunner, rName)
		if err != nil {
			return err
		}
		if nReq > int64(nRemaining) {
			return fmt.Errorf("failed to reserve enough devices: num_requested: %d, num_remaining: %d", nReq, nRemaining)
		}

		klog.Info("choosing devices")
		rDevs[rName], nodeState, err = ap.SelectDevices(ctx, isCardMgmtRunner, rName, nReq, nodeName)
		if err != nil {
			klog.ErrorS(err, "failed to select device(s)",
				"resource_name", rName, "num_requested", nReq, "num_remaining", nRemaining)
			return err
		}

		klog.Infof("devices selected: %v (node: %s, rName: %s, nReq: %v)", rDevs[rName], nodeName, rName, nReq)
	}

	klog.Info("reserving the chosen devices")
	for i := 0; i < spyrensUpdateTrialLimit; i++ {

		klog.Infof("reservation (prior): %v", nodeState.Status.Reservations)

		if nodeState.Status.Reservations == nil {
			nodeState.Status.Reservations = make(map[string]spyrev1alpha1.Reservation)
		}

		for rName, devs := range rDevs {
			r, exists := nodeState.Status.Reservations[rName]
			if exists {
				r.DeviceSets = append(r.DeviceSets, devs)
				r.PodsUnderScheduling = append(r.PodsUnderScheduling, spyrev1alpha1.Pod{Name: p.Name, Namespace: p.Namespace})
			} else {
				r.DeviceSets = [][]string{devs}
				r.PodsUnderScheduling = []spyrev1alpha1.Pod{{Name: p.Name, Namespace: p.Namespace}}
			}
			nodeState.Status.Reservations[rName] = r
		}
		klog.Infof("reservation: %v", nodeState.Status.Reservations)

		klog.Info("updating SpyreNodeState.Status (failure means some race condition)")
		// if updating status fails, it indicates that some race condition happened.
		if _, err := ap.spyreClient.UpdateStatus(ctx, nodeState, false); err == nil {
			klog.Info("reservation successfully finished")
			return nil
		}
		klog.Error(err)
	}
	err = fmt.Errorf("retry number exceeded")
	return err
}

// Unreserve deletes the devices which were reserved for the Pod from reservedSpyreInterfaces in SpyreNodeState.
func (ap *SpyrePlugin) Unreserve(ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) {
	klog.Infof("Unreserve (ignored): %s/%s", p.Namespace, p.Name)
}
