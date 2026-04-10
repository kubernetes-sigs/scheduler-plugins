/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	spyreconst "github.com/ibm-aiu/spyre-operator/const"
	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	"github.com/ibm-aiu/spyre-operator/pkg/types/pcitopov2"

	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type SpyrePlugin struct {
	spyreClient *spyreclient.SpyreClient
	k8sClient   client.Client
}

// following lines tell Go compiler that struct SpyrePlugin implements
// FilterPlugin and ReservePlugin interface; it is kind of assertion.
var _ framework.FilterPlugin = &SpyrePlugin{}
var _ framework.ReservePlugin = &SpyrePlugin{}

const (
	Name = "SpyrePlugin"
)

func (ap *SpyrePlugin) Name() string {
	return Name
}

func New(ctx context.Context, arg runtime.Object, h framework.Handle) (framework.Plugin, error) {

	klog.Info("creating spyreClient with InClusterConfig")
	spyreClient, err := spyreclient.NewClient(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create spyreClient: %w", err)
	}
	klog.Info("creating k8sClient with InClusterConfig")
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("Failed to get rest.InClusterConfig(): %w", err)
	}
	k8sClient, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate k8sClient: %w", err)
	}
	return &SpyrePlugin{spyreClient: spyreClient, k8sClient: k8sClient}, nil
}

func (ap *SpyrePlugin) SelectDevices(ctx context.Context, isPrivilegedPod bool, resourceName string,
	numReqDevs int64, nodeName string) ([]string, *spyrev1alpha1.SpyreNodeState, error) {

	nodeState, err := ap.spyreClient.GetSpyreNodeState(ctx, nodeName)
	if err != nil {
		klog.Errorf("failed to get SpyreNodeState for node: %v", nodeState)
		return nil, nil, err
	}
	for i, a := range nodeState.Status.AllocationList {
		klog.Infof("SelectDevices: nodeState.Allocation[%d]: {DeviceList: %v, Pod: %s/%s}",
			i, a.DeviceList, a.Pod.Namespace, a.Pod.Name)
	}
	klog.Infof("SelectDevices: nodeState.Reservation: %v", nodeState.Status.Reservations)

	if isSpecificDeviceRequest(resourceName) {
		return selectSpecificDevice(nodeState, isPrivilegedPod, resourceName)
	}

	// tier1/2 request must be 1 or even number
	if (strings.HasSuffix(resourceName, "_tier1") || strings.HasSuffix(resourceName, "_tier2")) &&
		numReqDevs > 1 && numReqDevs%2 != 0 {
		return nil, nil, fmt.Errorf("%s: requires even number devices", resourceName)
	}

	// get device tree of the node because
	tree, treeErr := deviceTree(nodeState, isPrivilegedPod, resourceName)

	// handle non-tier requests can be done regardless of pcitopo error
	if numReqDevs == 1 || !strings.Contains(resourceName, "_tier") {
		return selectAnyDevices(resourceName, isPrivilegedPod, numReqDevs, nodeState, tree)
	}

	// return at pcitopo error when request is topology-aware
	if treeErr != nil {
		return nil, nil,
			fmt.Errorf("unable to select %d %s device(s) due to pcitopo error: %w",
				numReqDevs, resourceName, treeErr)
	}

	// handle topology-aware requests with tree
	switch {
	case strings.HasSuffix(resourceName, "_tier0"):
		devs, err := selectTier0Devices(resourceName, numReqDevs, tree)
		return devs, nodeState, err
	case strings.HasSuffix(resourceName, "_tier1"):
		devs, err := selectTier1Devices(resourceName, numReqDevs, tree)
		return devs, nodeState, err
	case strings.HasSuffix(resourceName, "_tier2"):
		devs, err := selectTier2Devices(resourceName, numReqDevs, tree)
		return devs, nodeState, err
	default:
		err = fmt.Errorf("unexpected error for %d %s: %v", numReqDevs, resourceName, nodeState)
		return nil, nodeState, err
	}
}

// SelectDevicesForCardManagement selects devices for the first container of cardmgmt Pod.
// The VF address is simply derived by replacing PF device suffix (".0") with ".1".
// For example, if there are two PFs (00:01.0 and 00:02.0), then their first VFs (00:01.1 and 00.02.1)
// will be selected as well.
// This function will return error if any of the target PFs or VFs are not available.
func (ap *SpyrePlugin) SelectDevicesForCardManagement(pod *corev1.Pod, nodeState *spyrev1alpha1.SpyreNodeState, nodeName string) (map[string][]string, error) { //nolint:lll

	if len(nodeState.Spec.SpyreInterfaces) == 0 {
		return nil, fmt.Errorf("no spyreInterface exists on node: %s", nodeName)
	}

	numReq, err := getNumRequestedDevices(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to get number of requested devices: %w", err)
	}
	// cardmgmt only requests "spyre_pf" and "spyre_vf"
	// (does not request other types such as "spyre_pf_tier0", "spyre_pf_xxxx")
	numPfs := numReq[spyreconst.PfResourceName]
	numVfs := numReq[spyreconst.VfResourceName]

	pfs := make([]string, 0, numPfs)
	vfs := make([]string, 0, numVfs)

	for i := 0; len(pfs) < numPfs || len(vfs) < numVfs; i++ {
		if i == len(nodeState.Spec.SpyreInterfaces) {
			return nil,
				fmt.Errorf("not enough devices remain: %d/%d PFs and %d/%d VFs available/requested",
					len(pfs), numPfs, len(vfs), numVfs)
		}
		sIf := nodeState.Spec.SpyreInterfaces[i]
		if len(pfs) < numPfs && isAvailable(nodeState, sIf.PciAddress) {
			pfs = append(pfs, sIf.PciAddress)
		}
		vf := strings.Replace(sIf.PciAddress, ".0", ".1", 1)
		if len(vfs) < numVfs && isAvailable(nodeState, vf) {
			vfs = append(vfs, vf)
		}
	}
	rDevs := make(map[string][]string, 2)
	rDevs[spyreconst.PfResourceName] = pfs
	rDevs[spyreconst.VfResourceName] = vfs
	return rDevs, nil
}

func isSpecificDeviceRequest(resourceName string) bool {
	return strings.Count(resourceName, "_") >= 3
}

func selectAnyDevices(resourceName string, isPrivilegedPod bool, numReqDevs int64, nodeState *spyrev1alpha1.SpyreNodeState, tree [][][]string) ([]string, *spyrev1alpha1.SpyreNodeState, error) { //nolint:lll
	switch {
	case strings.HasPrefix(resourceName, spyreconst.PfResourceName):
		devs, err := selectAnyPfDevices(resourceName, numReqDevs, nodeState, tree)
		return devs, nodeState, err
	case strings.HasPrefix(resourceName, spyreconst.VfResourceName) && hasSSADevice(nodeState):
		devs, err := selectAnyIsolatedVfDevices(resourceName, numReqDevs, nodeState)
		return devs, nodeState, err
	case strings.HasPrefix(resourceName, spyreconst.VfResourceName):
		return selectAnyVfDevices(resourceName, isPrivilegedPod, numReqDevs, nodeState)
	default:
		err := fmt.Errorf("unsupported resource: %s", resourceName)
		return nil, nodeState, err
	}
}

func selectAnyIsolatedVfDevices(resourceName string, numReqDevs int64, nodeState *spyrev1alpha1.SpyreNodeState) ([]string, error) {
	selected := make([]string, 0, int(numReqDevs))

	// Select from SpyreSSAInterfaces directly
	for _, ssaInterface := range nodeState.Spec.SpyreSSAInterfaces {
		if ssaInterface.Health == spyrev1alpha1.SpyreHealthy {
			selected = append(selected, ssaInterface.PciAddress)
			if len(selected) == int(numReqDevs) {
				return selected, nil
			}
		}
	}

	return nil, fmt.Errorf("not enough isolated VFs remain (%s): %d requested, available: %d",
		resourceName, int(numReqDevs), len(selected))
}

// selectAnyPfDevices selects spyre_pf devices with no special condition.
// as similar to the tier0 request, select devices from smaller tier0 groups,
// and then select them from larger tier0 groups
// to keep large-size tier0 groups for future allocation request.
func selectAnyPfDevices(resourceName string, numReqDevs int64,
	nodeState *spyrev1alpha1.SpyreNodeState, tree [][][]string) ([]string, error) {

	devs := make([]string, 0, int(numReqDevs))

	// if tree is nil, simply selects available devices
	if tree == nil {
		for _, pf := range nodeState.Spec.SpyreInterfaces {
			if isAvailable(nodeState, pf.PciAddress) {
				devs = append(devs, pf.PciAddress)
				if len(devs) == int(numReqDevs) {
					return devs, nil
				}
			}
		}
		return nil,
			fmt.Errorf("not enough devices remain (%s): %d requested, %d available",
				resourceName, int(numReqDevs), len(devs))
	}

	// create tier0SizeList which stores sorted size list
	var t0SizeList []int
	for _, t1 := range tree {
		for _, t0 := range t1 {
			if len(t0) > 0 {
				t0SizeList = append(t0SizeList, len(t0))
			}
		}
	}
	sort.Ints(t0SizeList)

	// create t0BySize which stores pointers to t0.
	// example:
	//  if tree = { {{"a","b"},{"c"}}, { {"d","e","f"}, {"g"}} }
	//  then t0BySize =  { &[0][1], &[1][1], &[0][0], &[1][0] }
	// we don't care the order of {"c"} and {"g"}.
	t0BySize := make([]*[]string, len(t0SizeList))
	for _, t1 := range tree {
		for t0Idx := range t1 {
			for szIdx := range t0SizeList {
				if len(t1[t0Idx]) == t0SizeList[szIdx] && t0BySize[szIdx] == nil {
					t0BySize[szIdx] = &t1[t0Idx]
					break
				}
			}
		}
	}

	// finally, select devices from smaller-size t0 group.
	for _, pT0 := range t0BySize {
		for _, d := range *pT0 {
			devs = append(devs, d)
			if len(devs) == int(numReqDevs) {
				return devs, nil
			}
		}
	}
	return nil,
		fmt.Errorf("not enough devices remain (%s): %d requested, available: %v",
			resourceName, int(numReqDevs), tree)
}

func extractSpecifiedDevice(resourceName string) string {
	return strings.Join(strings.Split(resourceName, "_")[2:], ":")
}

// selectTier0Devices selects devices from a smallest-size tier0 group
// to keep large-size tier0 groups for future allocation request.
// ("s" is the size of t0; select smaller "s" t0 group)
func selectTier0Devices(resourceName string, numReqDevs int64, tree [][][]string) ([]string, error) {

	devs := make([]string, 0, int(numReqDevs))
	s := math.MaxInt
	for _, t1 := range tree {
		for _, t0 := range t1 {
			if len(t0) >= int(numReqDevs) && len(t0) < s {
				devs = t0[0:int(numReqDevs)]
				s = len(t0)
			}
		}
	}

	if len(devs) == int(numReqDevs) {
		return devs, nil
	} else {
		return nil,
			fmt.Errorf("not enough devices remain (%s): %d requested, available: %v",
				resourceName, int(numReqDevs), tree)
	}
}

// selectTier1Devices selects devices from two t0 groups because of end user's demand.
// To do so, tier0 request does not accept odd-number request.
func selectTier1Devices(resourceName string, numReqDevs int64, tree [][][]string) ([]string, error) {

	for _, t1 := range tree {
		firstTier0Idx := -1 // remember t0 where len(t0) >= numReqDevs / 2 as firstTier0Idx
		for idx, t0 := range t1 {
			if len(t0) >= int(numReqDevs)/2 {
				if firstTier0Idx == -1 {
					firstTier0Idx = idx
				} else {
					devs := make([]string, 0, int(numReqDevs))
					devs = append(devs, t1[firstTier0Idx][0:int(numReqDevs)/2]...)
					devs = append(devs, t1[idx][0:int(numReqDevs)/2]...)
					return devs, nil
				}
			}
		}
	}

	return nil,
		fmt.Errorf("not enough devices remain (%s): %d requested, available: %v",
			resourceName, int(numReqDevs), tree)
}

// selectTier2PfDevices selects devices from two t1 groups because of end user's demand
// as similar to [selectTier1PfDevices].
func selectTier2Devices(resourceName string, numReqDevs int64, tree [][][]string) ([]string, error) {

	firstTier1Idx := -1
	secondTier1Idx := -1
	devs := make([]string, 0, int(numReqDevs))
	for t1Idx, t1 := range tree {
		numAvailable := 0
		for _, t0 := range t1 {
			numAvailable += len(t0)
		}
		if numAvailable >= int(numReqDevs)/2 {
			if firstTier1Idx == -1 {
				firstTier1Idx = t1Idx
			} else {
				secondTier1Idx = t1Idx
				break
			}
		}
	}

	if firstTier1Idx == -1 || secondTier1Idx == -1 {
		return nil,
			fmt.Errorf("not enough devices remain (%s): %d requested, available: %v",
				resourceName, int(numReqDevs), tree)
	}

	for _, t1 := range [][][]string{tree[firstTier1Idx], tree[secondTier1Idx]} {
		firstTier0Idx := -1
		for idx, t0 := range t1 {
			if numReqDevs == 2 && len(t0) >= 2 {
				devs = append(devs, t1[idx][0])
				if len(devs) == int(numReqDevs) {
					return devs, nil
				}
				break // move to next t1
			}
			if len(t0) >= int(numReqDevs)/4 {
				if firstTier0Idx == -1 {
					firstTier0Idx = idx
				} else {
					devs = append(devs, t1[firstTier0Idx][0:int(numReqDevs)/4]...)
					devs = append(devs, t1[idx][0:int(numReqDevs)/4]...)
					if len(devs) == int(numReqDevs) {
						return devs, nil
					}
				}
			}
		}
	}

	return nil,
		fmt.Errorf("not enough devices remain (%s): %d requested, available: %v",
			resourceName, int(numReqDevs), tree)
}

func selectAnyVfDevices(resourceName string, isPrivilegedPod bool, numReqDevs int64, nodeState *spyrev1alpha1.SpyreNodeState) ([]string, *spyrev1alpha1.SpyreNodeState, error) { //nolint:lll
	nPf := len(nodeState.Spec.SpyreInterfaces)
	if nPf == 0 {
		return nil, nodeState, fmt.Errorf("spyreInterfaces is zero-length")
	}
	nVf1 := nodeState.Spec.SpyreInterfaces[0].NumVfs

	unavailableDevices := make([]string, 0, nPf*nVf1)
	for _, a := range nodeState.Status.AllocationList {
		unavailableDevices = append(unavailableDevices, a.DeviceList...)
	}
	if r, ok := nodeState.Status.Reservations[resourceName]; ok {
		for _, ds := range r.DeviceSets {
			unavailableDevices = append(unavailableDevices, ds...)
		}
	}

	selected := make([]string, 0, numReqDevs)

SelectDevice:
	for _, pf := range nodeState.Spec.SpyreInterfaces {
		for _, vf := range pf.Vfs {
			if strings.HasSuffix(vf, ".1") && !isPrivilegedPod {
				continue
			}
			if !slices.Contains(unavailableDevices, vf) {
				selected = append(selected, vf)
				if len(selected) == int(numReqDevs) {
					break SelectDevice
				}
				break // to check next PF
			}
		}
	}

	if int64(len(selected)) < numReqDevs {
		return nil, nodeState, fmt.Errorf("not enough VFs remain (%d remains for %d request)", len(selected), numReqDevs)
	}
	return selected, nodeState, nil
}

// selectSpecificDevice selects an arbitrary VF device from PF device embedded in requested resource name.
func selectSpecificDevice(nodeState *spyrev1alpha1.SpyreNodeState,
	isPrivilegedPod bool, resourceName string) ([]string, *spyrev1alpha1.SpyreNodeState, error) {
	reqDevId := extractSpecifiedDevice(resourceName)
	d := availableDevice(nodeState, isPrivilegedPod, resourceName, reqDevId)

	if len(d) == 0 {
		return nil, nodeState, fmt.Errorf("device unavailable: %s", resourceName)
	}
	return []string{d}, nodeState, nil
}

// isAvailable returns true if following conditions are met.
// - device health is healthy
// - device is neither allocated nor reserved
// if conditions are not met, returns false.
func isAvailable(nodeState *spyrev1alpha1.SpyreNodeState, d string) bool {
	if len(d) == 0 { // this happens when no peer2 device exists.
		return false
	}

	// use linear search to find unhealthy devices
	// because the length of unhealthyDevices is usually zero.
	for _, u := range nodeState.Status.UnhealthyDevices {
		if d == u.ID {
			return false
		}
	}

	// Check SpyreSSAInterfaces
	for _, ssaInterface := range nodeState.Spec.SpyreSSAInterfaces {
		if ssaInterface.PciAddress == d && ssaInterface.Health != spyrev1alpha1.SpyreHealthy {
			return false
		}
	}
	for _, a := range nodeState.Status.AllocationList {
		if slices.Contains(a.DeviceList, d) {
			return false
		}
	}
	for _, r := range nodeState.Status.Reservations {
		for _, ds := range r.DeviceSets {
			if slices.Contains(ds, d) {
				return false
			}
		}
	}
	return true
}

// availableDevice returns PF/VF device ID or empty string.
//
// If resourceName starts with `spyre_pf`, it returns d if it is available.
// returns empty string if no available device is found.
//
// If resourceName starts with `spyre_vf`, its decision varies on whether
// requester Pod is user Pod or privileged Pod like card-management Pod.
// If isPrivilegedPod is true, it searches all of the VFs from the PF
// specified as d, and returns one of the available VFs of the PF.
// If isPrivilegedPod is false, it does same operation but excludes first
// VF device (`XXXX.XX.1`) because it is (or it will be) used by
// privileged Pods.
func availableDevice(nodeState *spyrev1alpha1.SpyreNodeState, isPrivilegedPod bool, resourceName string, d string) string {

	switch {
	case strings.HasPrefix(resourceName, spyreconst.PfResourceName):
		if isAvailable(nodeState, d) {
			return d
		}

	case strings.HasPrefix(resourceName, spyreconst.VfResourceName):
		// Check if this is an isolated VF from SpyreSSAInterfaces
		if hasSSADevice(nodeState) {
			for _, ssaInterface := range nodeState.Spec.SpyreSSAInterfaces {
				if ssaInterface.PciAddress == d && ssaInterface.Health == spyrev1alpha1.SpyreHealthy && isAvailable(nodeState, d) {
					return d
				}
			}
		} else {
			// Handle regular VFs from SpyreInterfaces
			for _, spyreIf := range nodeState.Spec.SpyreInterfaces {
				if spyreIf.PciAddress == d {
					if spyreIf.Health != spyrev1alpha1.SpyreHealthy {
						return ""
					}
					for _, vf := range spyreIf.Vfs {
						if strings.HasSuffix(vf, ".1") && !isPrivilegedPod {
							continue
						}
						if isAvailable(nodeState, vf) {
							return vf
						}
					}
				}

				if spyreIf.PciAddress == d && len(spyreIf.Vfs) == 0 && isAvailable(nodeState, d) {
					return d
				}
			}
		}
	}
	return ""
}

// getNumRemainingDevices returns the number of remaining devices, which is calculated by
// num_devices - num_allocated_devices - num_reserved_devices
// If the requester is a privileged Pod, this function only counts devices ending with `.1`.
func (ap *SpyrePlugin) getNumRemainingDevices(ctx context.Context, nodeState *spyrev1alpha1.SpyreNodeState, isPrivileged bool, resourceName string) (int, error) { //nolint:lll

	// Count SpyreSSAInterfaces for isolated VF requests
	if strings.HasPrefix(resourceName, spyreconst.VfResourceName) && hasSSADevice(nodeState) {
		count := 0
		for _, ssaInterface := range nodeState.Spec.SpyreSSAInterfaces {
			if isAvailable(nodeState, ssaInterface.PciAddress) {
				count++
			}
		}
		return count, nil
	}

	if len(nodeState.Spec.SpyreInterfaces) == 0 {
		return 0, nil
	}
	// use linear search to avoid memory consumption of map because
	// in our usecase we assume the maximum number of VFs is around 200.

	count := 0
	for _, pf := range nodeState.Spec.SpyreInterfaces {
		if strings.HasPrefix(resourceName, spyreconst.PfResourceName) {
			if isAvailable(nodeState, pf.PciAddress) {
				count++
			}
		} else if strings.HasPrefix(resourceName, spyreconst.VfResourceName) {
			// Only count VFs from SpyreInterfaces (not isolated VFs)
			for _, vf := range pf.Vfs {
				if isAvailable(nodeState, vf) {
					if !isPrivileged {
						count++
						break // only count one VF per PF
					} else if strings.HasSuffix(vf, ".1") {
						count++
						break
					}
				}
			}
		}
	}

	klog.Infof("getNumRemainingDevices: using regular path, count=%d, resourceName=%s", count, resourceName)
	return count, nil
}

// cleanupOrphanReservation removes staled reservation entries. "staled" means:
//
//  1. `podUnderScheduling` entries which do not exist in the cluster - this Pod became
//     error and no longer exists.
//  2. `podUnderScheduling` entry same as the to-be-scheduled Pod - this Pod is being
//     re-created and therefore previous reservation must be removed.
//
// `deviceSets` entry that its size matches `.spec.containers[].resources.requests` or
// `.spec.containers[].resources.requests` of each removed `podUnderScheduling` entry
// is also removed. (e.g., a staled Pod requests 4 devices, then one of the `deviceSets`
// entry which has 4 devices will also be removed)
func (ap *SpyrePlugin) cleanupOrphanReservation(ctx context.Context, nodeName string, schedPod *corev1.Pod) (*spyrev1alpha1.SpyreNodeState, error) { //nolint:lll

	var nodeState *spyrev1alpha1.SpyreNodeState

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {

		var err error
		klog.Infof("getting SpyreNodeState for node: %s", nodeName)
		nodeState, err = ap.spyreClient.GetSpyreNodeState(ctx, nodeName)
		if err != nil {
			klog.Errorf("failed to get NodeState: %s", err)
			return err
		}

		klog.Infof("cleaning-up orphan reservation")
		ns2PodList := make(map[string]*corev1.PodList)
		for resName, r := range nodeState.Status.Reservations {
			if len(r.PodsUnderScheduling) == 0 {
				klog.Infof("cleaning-up DeviceSets: %v", r.DeviceSets)
				nodeState.Status.Reservations[resName] = spyrev1alpha1.Reservation{}
				continue
			}

			newPusList := []spyrev1alpha1.Pod{}
			devSetPreserveFlag := make([]bool, len(r.DeviceSets))
			for _, pus := range r.PodsUnderScheduling {

				// Don't preserve a Pod entry which name is same as
				// the Pod to-be-scheduled because it is now being re-created.
				if pus.Name == schedPod.Name && pus.Namespace == schedPod.Namespace {
					klog.Infof("PodUnderScheduling %s/%s is removed because it is now being re-created",
						pus.Namespace, pus.Name)
					continue // don't add it to the preserve list (pList)
				}

				// get Pod list (once per namespace)
				pList, exists := ns2PodList[pus.Namespace]
				if !exists {
					ns2PodList[pus.Namespace] = &corev1.PodList{}
					opt := &client.ListOptions{Namespace: pus.Namespace}
					ctx := context.Background()
					err := ap.k8sClient.List(ctx, ns2PodList[pus.Namespace], opt)
					if err != nil {
						klog.Errorf("failed to get Pod list in namespace %s: %s", pus.Namespace, err.Error())
						return err
					}
					pList = ns2PodList[pus.Namespace]
				}

				// preserve 1) Pod in podUnderScheduling only if it exists in a namespace
				// and 2) deviceSet entry which has same number of devices to the Pod.
				found := false
				for _, p := range pList.Items {
					if pus.Name == p.Name {
						newPusList = append(newPusList, pus)
						rName := corev1.ResourceName(spyreconst.ResourcePrefix + "/" + resName)
						var q resource.Quantity
						var exists bool
						for _, c := range p.Spec.Containers {
							q, exists = c.Resources.Requests[rName]
							if !exists {
								q, exists = c.Resources.Limits[rName]
							}
							if exists {
								break
							}
						}
						for i, hasAlreadyPreserved := range devSetPreserveFlag {
							if (&q).CmpInt64(int64(len(r.DeviceSets[i]))) == 0 &&
								!hasAlreadyPreserved {
								devSetPreserveFlag[i] = true
								break
							}
						}
						found = true
						break
					}
				}

				if !found {
					klog.Infof("PodUnderScheduling %s/%s no longer exists", pus.Namespace, pus.Name)
				}
			}
			newDevSets := make([][]string, 0, len(r.DeviceSets))
			for i := range r.DeviceSets {
				if devSetPreserveFlag[i] {
					newDevSets = append(newDevSets, r.DeviceSets[i])
				}
			}
			r.DeviceSets = newDevSets
			r.PodsUnderScheduling = newPusList
			klog.Infof("Remove DeviceSets")
			nodeState.Status.Reservations[resName] = r
		}
		nodeState, err = ap.spyreClient.UpdateStatus(ctx, nodeState, true)
		return err
	})

	return nodeState, err
}

// getNumRequestedDevices returns the number and the resource name of devices that
// a Pod requests in various forms.
// This function assumes that a Pod requests one of PF devices and/or one of VF
// devices. For example, "spyre_pf" and "spyre_vf", "spyre_pf_tier0" and "spyre_vf".
// Pods requesting multiple PF/VF (like "spyre_pf" and "spyre_pf_tier0") are not
// acceptable.
func getNumRequestedDevices(p *corev1.Pod) (map[string]int, error) {

	pf := spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName
	vf := spyreconst.ResourcePrefix + "/" + spyreconst.VfResourceName

	var numPfs int
	var pfName string
	var numVfs int
	var vfName string

	for _, c := range p.Spec.Containers {
		var resList corev1.ResourceList

		switch {
		case c.Resources.Limits != nil:
			resList = c.Resources.Limits
		case c.Resources.Requests != nil:
			resList = c.Resources.Requests
		default:
			continue // this container does not request SPYRE.
		}

		for k, v := range resList {
			if !(strings.HasPrefix(string(k), pf) || strings.HasPrefix(string(k), vf)) {
				continue
			}
			q, ok := v.AsInt64()
			if !ok {
				return nil, fmt.Errorf("failed to get the quantity of resource request: %s", v.String())
			}
			a := strings.Split(string(k), "/")
			name := a[len(a)-1]
			switch {
			case strings.HasPrefix(string(k), pf):
				numPfs += int(q)
				pfName = name
			case strings.HasPrefix(string(k), vf):
				numVfs += int(q)
				vfName = name
			}
		}
	}

	m := map[string]int{}
	if numPfs > 0 {
		m[pfName] = numPfs
	}
	if numVfs > 0 {
		m[vfName] = numVfs
	}
	return m, nil

}

// deviceTree returns groups of peer0 available devices.
// For example, {{{a,b,c}, {d,e}}, {{f,g},{h,i}}} and c, g has already been allocated, then
// this function returns {{{a,b},{d,e}},{{f},{h,i}}}
// if resourceName starts with `spyre_vf`, the tree contains VFs. One VF device is selected
// for each PF.
func deviceTree(nodeState *spyrev1alpha1.SpyreNodeState, isPrivilegedPod bool, resourceName string) ([][][]string, error) {
	s := nodeState.Spec.Pcitopo
	var pcitopo pcitopov2.Pcitopo
	err := json.Unmarshal(([]byte)(s), &pcitopo)
	if err != nil {
		return nil, err
	}

	// construct simple tree if all devices are connected as peer2 devices,
	// which is a standard topology in IBMz systems.
	isIsolatedVF := strings.HasPrefix(resourceName, "spyre_vf") && hasSSADevice(nodeState)

	if len(pcitopo.Devices) > 0 || (len(pcitopo.SpyreVfDevices) > 0 && isIsolatedVF) {
		var k string
		var peers pcitopov2.Peers
		if strings.HasPrefix(resourceName, "spyre_vf") && len(pcitopo.SpyreVfDevices) > 0 {
			k = maps.Keys(pcitopo.SpyreVfDevices)[0]
			peers = pcitopo.SpyreVfDevices[k].SpyreVfPeers
		} else { // "spyre_pf" or topology without "spyre_vf_devices"
			k = maps.Keys(pcitopo.Devices)[0]
			peers = pcitopo.Devices[k].Peers
		}
		if len(peers.Peer0) == 0 && len(peers.Peer1) == 0 {
			devs := append(maps.Keys(peers.Peer2), k)
			availableDevs := make([]string, 0, len(devs))
			for _, dev := range devs {
				if d := availableDevice(nodeState, isPrivilegedPod, resourceName, dev); len(d) > 0 {
					availableDevs = append(availableDevs, d)
				}
			}
			return [][][]string{{availableDevs, {}}, {}}, nil
		}
	}

	var (
		devices  map[string]pcitopov2.Device
		getPeers func(pcitopov2.Device) pcitopov2.Peers
	)

	if isIsolatedVF {
		if len(pcitopo.SpyreVfDevices) == 0 {
			return [][][]string{}, nil
		}
		devices = pcitopo.SpyreVfDevices
		// SpyreVfPeers can be used to find tier resource only if there is only one vf per pf.
		getPeers = func(d pcitopov2.Device) pcitopov2.Peers { return d.SpyreVfPeers }
	} else {
		if len(pcitopo.Devices) == 0 {
			return [][][]string{}, nil
		}
		devices = pcitopo.Devices
		getPeers = func(d pcitopov2.Device) pcitopov2.Peers { return d.Peers }
	}

	// ── create first tier1 group ──
	var g1 [][]string
	var devNext string

	devList := maps.Keys(devices)
	sort.Strings(devList)
	d := devList[0]
	devInfo := devices[d]
	peers := getPeers(devInfo)

	t0 := make([]string, 0, len(peers.Peer0)+1)
	if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, d); len(devId) > 0 {
		t0 = append(t0, devId)
	}
	p0 := maps.Keys(peers.Peer0)
	sort.Strings(p0)
	for _, id := range p0 {
		if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, id); len(devId) > 0 {
			t0 = append(t0, devId)
		}
	}

	t1 := make([]string, 0, len(peers.Peer1))
	p1 := maps.Keys(peers.Peer1)
	sort.Strings(p1)
	for _, id := range p1 {
		if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, id); len(devId) > 0 {
			t1 = append(t1, devId)
		}
	}
	g1 = [][]string{t0, t1}

	p2 := maps.Keys(peers.Peer2)
	if len(p2) == 0 {
		return [][][]string{g1}, nil
	}
	sort.Strings(p2)
	devNext = p2[0]

	// ── create second tier1 group ──
	devInfo = devices[devNext]
	peers = getPeers(devInfo)

	t0 = make([]string, 0, len(peers.Peer0)+1)
	if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, devNext); len(devId) > 0 {
		t0 = append(t0, devId)
	}
	p0 = maps.Keys(peers.Peer0)
	sort.Strings(p0)
	for _, id := range p0 {
		if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, id); len(devId) > 0 {
			t0 = append(t0, devId)
		}
	}

	t1 = make([]string, 0, len(peers.Peer1))
	p1 = maps.Keys(peers.Peer1)
	sort.Strings(p1)
	for _, id := range p1 {
		if devId := availableDevice(nodeState, isPrivilegedPod, resourceName, id); len(devId) > 0 {
			t1 = append(t1, devId)
		}
	}
	g2 := [][]string{t0, t1}

	return [][][]string{g1, g2}, nil
}

func isCardManagementPod(pod *corev1.Pod, operatorNs string) bool {
	return pod.Labels[spyreconst.CardManagementLabelKey] == spyreconst.CardManagementLabelValue && pod.Namespace == operatorNs
}

func isCardManagementRunnerPod(pod *corev1.Pod, operatorNs string) bool {
	return pod.Labels[spyreconst.CardManagementRunnerLabelKey] == spyreconst.CardManagementRunnerLabelValue && pod.Namespace == operatorNs
}

func hasSSADevice(nodeState *spyrev1alpha1.SpyreNodeState) bool {
	// Check if there are any SpyreSSAInterfaces (isolated VFs) AND no regular VFs
	// This ensures we only use SSA path for pure isolated VF scenarios
	hasSSA := len(nodeState.Spec.SpyreSSAInterfaces) > 0
	hasRegularVFs := false

	for _, pf := range nodeState.Spec.SpyreInterfaces {
		if len(pf.Vfs) > 0 {
			hasRegularVFs = true
			break
		}
	}

	result := hasSSA && !hasRegularVFs
	return result
}
