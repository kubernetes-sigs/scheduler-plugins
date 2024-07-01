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
	v1 "k8s.io/api/core/v1"
	res "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

type NodeInfo struct {
	DisksStatus   map[string]*DiskInfo // key is disk device id and the value is diskStatus
	DefaultDevice string               // key is disk device id
}

type DiskInfo struct {
	NormalizerName string
	DiskName       string
	Capacity       v1alpha1.IOBandwidth
	Allocatable    v1alpha1.IOBandwidth
}

func (d *DiskInfo) DeepCopy() *DiskInfo {
	newDiskInfo := &DiskInfo{
		DiskName:       d.DiskName,
		Capacity:       d.Capacity,
		Allocatable:    d.Allocatable,
		NormalizerName: d.NormalizerName,
	}

	return newDiskInfo
}

type Resource struct {
	nodeName string
	info     *NodeInfo
	ch       resource.CacheHandle
	sync.RWMutex
}

func (ps *Resource) Name() string {
	return "BlockIO"
}

func (ps *Resource) AddPod(pod *v1.Pod, request v1alpha1.IOBandwidth) error {
	ps.Lock()
	defer ps.Unlock()
	dev := ps.info.DefaultDevice
	if _, ok := ps.info.DisksStatus[dev]; !ok {
		return fmt.Errorf("cannot find default device %s in cache", dev)
	}
	if ps.info.DisksStatus[dev].Allocatable.Read.Cmp(request.Read) < 0 {
		ps.info.DisksStatus[dev].Allocatable.Read = res.MustParse("0")
	} else {
		ps.info.DisksStatus[dev].Allocatable.Read.Sub(request.Read)
	}
	if ps.info.DisksStatus[dev].Allocatable.Write.Cmp(request.Write) < 0 {
		ps.info.DisksStatus[dev].Allocatable.Write = res.MustParse("0")
	} else {
		ps.info.DisksStatus[dev].Allocatable.Write.Sub(request.Write)
	}
	if ps.info.DisksStatus[dev].Allocatable.Total.Cmp(request.Total) < 0 {
		ps.info.DisksStatus[dev].Allocatable.Total = res.MustParse("0")
	} else {
		ps.info.DisksStatus[dev].Allocatable.Total.Sub(request.Total)
	}
	return nil
}

func (ps *Resource) RemovePod(pod *v1.Pod) error {
	resource.IoiContext.Lock()
	request, err := resource.IoiContext.GetPodRequest(string(pod.UID))
	if err != nil {
		return fmt.Errorf("cannot get pod request: %v", err)
	}
	resource.IoiContext.Unlock()
	ps.Lock()
	defer ps.Unlock()
	dev := ps.info.DefaultDevice
	if _, ok := ps.info.DisksStatus[dev]; !ok {
		return fmt.Errorf("cannot find default device %s in cache", dev)
	}
	ps.info.DisksStatus[dev].Allocatable.Read.Add(request.Read)
	if ps.info.DisksStatus[dev].Allocatable.Read.Cmp(ps.info.DisksStatus[dev].Capacity.Read) > 0 {
		ps.info.DisksStatus[dev].Allocatable.Read = ps.info.DisksStatus[dev].Capacity.Read
	}
	ps.info.DisksStatus[dev].Allocatable.Write.Add(request.Write)
	if ps.info.DisksStatus[dev].Allocatable.Write.Cmp(ps.info.DisksStatus[dev].Capacity.Write) > 0 {
		ps.info.DisksStatus[dev].Allocatable.Write = ps.info.DisksStatus[dev].Capacity.Write
	}
	ps.info.DisksStatus[dev].Allocatable.Total.Add(request.Total)
	if ps.info.DisksStatus[dev].Allocatable.Total.Cmp(ps.info.DisksStatus[dev].Capacity.Total) > 0 {
		ps.info.DisksStatus[dev].Allocatable.Total = ps.info.DisksStatus[dev].Capacity.Total
	}
	return nil
}

func (ps *Resource) PrintInfo() {
	for disk, diskInfo := range ps.info.DisksStatus {
		klog.V(2).Info("device id: ", disk)
		klog.V(2).Info("device name: ", diskInfo.DiskName)
		klog.V(2).Info("normalizer name: ", diskInfo.NormalizerName)
		klog.V(2).Info("capacity read: ", diskInfo.Capacity.Read.String())
		klog.V(2).Info("capacity write: ", diskInfo.Capacity.Write.String())
		klog.V(2).Info("capacity total: ", diskInfo.Capacity.Total.String())
		klog.V(2).Info("allocatable read: ", diskInfo.Allocatable.Read.String())
		klog.V(2).Info("allocatable write: ", diskInfo.Allocatable.Write.String())
		klog.V(2).Info("allocatable total: ", diskInfo.Allocatable.Total.String())
	}
}
