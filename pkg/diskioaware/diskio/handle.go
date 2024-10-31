/*
Copyright 2024 The Kubernetes Authors.

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

package diskio

import (
	"fmt"
	"sync"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

type Handle struct {
	resource.HandleBase
	client kubernetes.Interface
	sync.RWMutex
}

// ioiresource.Handle interface
func (h *Handle) Name() string {
	return "DiskIOHandle"
}

func (h *Handle) Run(c resource.ExtendedCache, cli kubernetes.Interface) error {
	h.EC = c
	h.client = cli
	return nil
}

// New Resource Handle
func New() resource.Handle {
	return &Handle{}
}

func (h *Handle) AddCacheNodeInfo(node string, disks map[string]v1alpha1.DiskDevice) {
	nodeInfo := &NodeInfo{
		DisksStatus: make(map[string]*DiskInfo),
	}
	for disk, info := range disks {
		nodeInfo.DisksStatus[disk] = &DiskInfo{
			DiskName:       disk,
			NormalizerName: fmt.Sprintf("%s-%s", info.Vendor, info.Model),
			Capacity: v1alpha1.IOBandwidth{
				Read:  info.Capacity.Read.DeepCopy(),
				Write: info.Capacity.Write.DeepCopy(),
				Total: info.Capacity.Total.DeepCopy(),
			},
			Allocatable: v1alpha1.IOBandwidth{
				Read:  info.Capacity.Read.DeepCopy(),
				Write: info.Capacity.Write.DeepCopy(),
				Total: info.Capacity.Total.DeepCopy(),
			},
		}
		if info.Type == string(common.EmptyDir) {
			nodeInfo.DefaultDevice = disk
		}
	}
	h.Lock()
	defer h.Unlock()
	h.EC.SetExtendedResource(node, &Resource{
		nodeName: node,
		info:     nodeInfo,
		ch:       h})
	h.EC.PrintCacheInfo()
}

func (h *Handle) DeleteCacheNodeInfo(nodeName string) error {
	h.Lock()
	defer h.Unlock()
	h.EC.DeleteExtendedResource(nodeName)
	h.EC.PrintCacheInfo()
	return nil
}
func (h *Handle) UpdateCacheNodeStatus(nodeName string, nodeIoBw v1alpha1.NodeDiskIOStatsStatus) error {
	r, err := h.getResource(nodeName)
	if err != nil {
		return err
	}
	r.Lock()
	defer r.Unlock()
	for dev, bw := range nodeIoBw.AllocatableBandwidth {
		r.info.DisksStatus[dev].Allocatable = v1alpha1.IOBandwidth{
			Read:  bw.Read.DeepCopy(),
			Write: bw.Write.DeepCopy(),
			Total: bw.Total.DeepCopy(),
		}
	}
	h.EC.PrintCacheInfo()
	return nil
}
func (h *Handle) IsIORequired(annotations map[string]string) bool {
	if _, ok := annotations[common.DiskIOAnnotation]; ok {
		return true
	}
	return false

}
func (h *Handle) CanAdmitPod(nodeName string, req v1alpha1.IOBandwidth) (bool, error) {
	r, err := h.getResource(nodeName)
	if err != nil {
		return false, err
	}
	r.Lock()
	defer r.Unlock()
	dev := r.info.DefaultDevice
	if _, ok := r.info.DisksStatus[dev]; !ok {
		return false, fmt.Errorf("emptydir disk %v has not been registered in cache", dev)
	}
	if r.info.DisksStatus[dev].Allocatable.Read.Cmp(req.Read) < 0 {
		return false, fmt.Errorf("node %v disk IO read bandwidth is not enough", nodeName)
	}
	if r.info.DisksStatus[dev].Allocatable.Write.Cmp(req.Write) < 0 {
		return false, fmt.Errorf("node %v disk IO write bandwidth is not enough", nodeName)
	}
	if r.info.DisksStatus[dev].Allocatable.Total.Cmp(req.Total) < 0 {
		return false, fmt.Errorf("node %v disk IO total bandwidth is not enough", nodeName)
	}
	return true, nil
}

func (h *Handle) NodePressureRatio(node string, request v1alpha1.IOBandwidth) (float64, error) {
	r, err := h.getResource(node)
	if err != nil {
		return 0, err
	}
	r.Lock()
	defer r.Unlock()
	dev := r.info.DefaultDevice
	if _, ok := r.info.DisksStatus[dev]; !ok {
		return 0, fmt.Errorf("emptydir disk %v has not been registered in cache", dev)
	}

	rAllocatable := r.info.DisksStatus[dev].Allocatable.Read.AsApproximateFloat64()
	rRequested := request.Read.AsApproximateFloat64()
	wAllocatable := r.info.DisksStatus[dev].Allocatable.Write.AsApproximateFloat64()
	wRequested := request.Write.AsApproximateFloat64()
	return (rRequested/rAllocatable + wRequested/wAllocatable) / 2, nil
}

func (h *Handle) GetDiskNormalizeModel(node string) (string, error) {
	r, err := h.getResource(node)
	if err != nil {
		return "", err
	}
	dev := r.info.DefaultDevice
	if _, ok := r.info.DisksStatus[dev]; !ok {
		return "", fmt.Errorf("emptydir disk %v not registered in cache", dev)
	}
	return r.info.DisksStatus[dev].NormalizerName, nil
}

func (h *Handle) getResource(node string) (*Resource, error) {
	h.RLock()
	rs := h.EC.GetExtendedResource(node)
	if rs == nil {
		return nil, fmt.Errorf("node %v has not been registered in cache", node)
	}
	h.RUnlock()
	r, ok := rs.(*Resource)
	if !ok {
		return nil, fmt.Errorf("incorrect resource cached")
	}
	return r, nil
}
