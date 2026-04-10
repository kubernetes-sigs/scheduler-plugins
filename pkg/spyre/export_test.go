/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre

import (
	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var IsAvailable = isAvailable
var GetNumRequestedDevices = getNumRequestedDevices
var GetNumRemainingDevices = (*SpyrePlugin).getNumRemainingDevices
var ReserveDevices = (*SpyrePlugin).reserveDevices
var DeviceTree = deviceTree
var CleanupOrphanReservation = (*SpyrePlugin).cleanupOrphanReservation
var ExportFilter = (*SpyrePlugin).filter
var SelectDevices = (*SpyrePlugin).SelectDevices
var SelectDevicesForCardManagement = (*SpyrePlugin).SelectDevicesForCardManagement
var IsCardManagementPod = isCardManagementPod
var IsCardManagementRunnerPod = isCardManagementRunnerPod
var HasSSADevice = hasSSADevice

func (ap *SpyrePlugin) ExportSetSpyreClient(ac *spyreclient.SpyreClient) {
	ap.spyreClient = ac
}
func (ap *SpyrePlugin) ExportSetK8sClient(kc client.Client) {
	ap.k8sClient = kc
}
