/*
Copyright 2024 Intel Corporation

Licensed under the Apache License, Version 2.0 (the License);
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package iodriver

import (
	"context"
	"fmt"
	"log"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clientretry "k8s.io/client-go/util/retry"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	externalinformer "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/informers/externalversions"
	"k8s.io/client-go/tools/cache"
)

func (c *IODriver) WatchNodeDiskIOStats(ctx context.Context) error {
	nodeInfoInformerFactory := externalinformer.NewSharedInformerFactory(c.excs, 0)

	statusInfoInformer := nodeInfoInformerFactory.Diskio().V1alpha1().NodeDiskIOStatses().Informer()
	statusInfoHandler := cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			info, ok := obj.(*v1alpha1.NodeDiskIOStats)
			if !ok {
				log.Printf("cannot convert to *v1alpha1.NodeDiskIOStats: %v", obj)
				return false
			}
			if info.Spec.NodeName != c.nodeName {
				return false
			}
			return true
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.AddNodeDiskIOStatsCR,
			UpdateFunc: c.UpdateNodeDiskIOStatsCR,
		},
	}
	if _, err := statusInfoInformer.AddEventHandler(statusInfoHandler); err != nil {
		return err
	}

	nodeInfoInformerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), statusInfoInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync NodeDiskIOStats")
	}

	return nil
}

func (c *IODriver) AddNodeDiskIOStatsCR(obj interface{}) {
	nodeStatus, ok := obj.(*v1alpha1.NodeDiskIOStats)
	if !ok {
		log.Printf("[AddNodeDiskIOStatsCR]cannot convert oldObj to *v1alpha1.NodeDiskIOStats: %v", obj)
		return
	}

	if err := c.processDiskCrData(nodeStatus); err != nil {
		log.Printf("failed to send podInfo to channel: %v", err)
		return
	}
	log.Printf("[AddNodeDiskIOStatsCR] node %v is handled", nodeStatus.Spec.NodeName)
}

func (c *IODriver) UpdateNodeDiskIOStatsCR(oldObj, newObj interface{}) {
	oldStatus, ok := oldObj.(*v1alpha1.NodeDiskIOStats)
	if !ok {
		log.Printf("[UpdateNodeDiskIOStatsCR]cannot convert oldObj to *v1alpha1.NodeDiskIOStats: %v", oldObj)
		return
	}
	newStatus, ok := newObj.(*v1alpha1.NodeDiskIOStats)
	if !ok {
		log.Printf("[UpdateNodeDiskIOStatsCR]cannot convert newObj to *v1alpha1.NodeDiskIOStats: %v", newObj)
		return
	}
	// podList has update
	if !reflect.DeepEqual(oldStatus.Spec, newStatus.Spec) {
		if err := c.processDiskCrData(newStatus); err != nil {
			log.Printf("failed to process CR data: %v", err)
			return
		}
	}
	log.Printf("[UpdateNodeDiskIOStatsCR] node %v is handled", newStatus.Spec.NodeName)
}

func (c *IODriver) processDiskCrData(data *v1alpha1.NodeDiskIOStats) error {
	if data == nil {
		return fmt.Errorf("data is nil")
	}
	if c.diskInfos == nil || len(c.diskInfos.EmptyDir) == 0 {
		return fmt.Errorf("diskInfos is nil")
	}

	if c.observedGeneration != nil && data.Generation <= *c.observedGeneration {
		return fmt.Errorf("the generation is earlier than the one in cache")
	}

	// get pods' annotation on the node
	anns, err := c.getPodAnnOnNode()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.observedGeneration = &data.Generation

	log.Printf("now in ProcessDiskCrData generation:%d ", *c.observedGeneration)
	visited := sets.Set[string]{} // podId
	for _, podId := range data.Spec.ReservedPods {
		visited.Insert(podId)
		if pInfo, ok := c.podCrInfos[podId]; !ok || pInfo == nil {
			ioAnn, ok := anns[podId]
			if !ok {
				ioAnn = ""
			}
			bw, err := EstimateRequest(ioAnn)
			if err != nil {
				return err
			}
			c.podCrInfos[podId] = &PodCrInfo{
				devId: c.diskInfos.EmptyDir,
				bw:    bw,
			}
		}
	}

	for podId := range c.podCrInfos {
		if !visited.Has(podId) {
			// delete operation
			delete(c.podCrInfos, podId)
		}
	}
	bw, err := c.calDiskAllocatable(c.diskInfos.EmptyDir)
	if err != nil {
		return err
	}
	c.mu.Unlock()

	toUpdate := make(map[string]v1alpha1.IOBandwidth)
	toUpdate[c.diskInfos.EmptyDir] = *bw
	err = c.UpdateNodeDiskIOInfoStatus(context.Background(), toUpdate)
	if err != nil {
		return err
	}
	return nil
}

func (c *IODriver) getPodAnnOnNode() (map[string]string, error) {
	ann := make(map[string]string)
	pods, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == c.nodeName || pod.Spec.NodeName == "" {
			ba, ok := pod.Annotations[DiskIOAnnotation]
			if ok {
				ann[string(pod.UID)] = ba
			} else {
				ann[string(pod.UID)] = ""
			}
		}
	}
	return ann, nil
}

func (e *IODriver) GetNodeDiskIOStats(ctx context.Context) (*v1alpha1.NodeDiskIOStats, error) {
	if e.excs == nil {
		return nil, fmt.Errorf("client cannot be nil")
	}
	if len(e.nodeName) == 0 {
		return nil, fmt.Errorf("node name cannot be empty")
	}
	obj, err := e.excs.DiskioV1alpha1().NodeDiskIOStatses(CRNameSpace).Get(ctx, GetCRName(e.nodeName, NodeDiskIOInfoCRSuffix), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (e *IODriver) UpdateNodeDiskIOInfoStatus(ctx context.Context, toUpdate map[string]v1alpha1.IOBandwidth) error {
	return clientretry.RetryOnConflict(UpdateBackoff, func() error {
		var err error
		var sts *v1alpha1.NodeDiskIOStats

		sts, err = e.GetNodeDiskIOStats(ctx)
		if err != nil {
			return err
		}

		// Make a copy of the pod and update the annotations.
		newSts := sts.DeepCopy()
		// todo: newSts.Status will not be nil
		newSts.Status.ObservedGeneration = e.observedGeneration
		if newSts.Status.AllocatableBandwidth == nil {
			newSts.Status.AllocatableBandwidth = make(map[string]v1alpha1.IOBandwidth)
		}
		for key, val := range toUpdate {
			if value, ok := newSts.Status.AllocatableBandwidth[key]; ok && reflect.DeepEqual(value, val) {
				return nil
			} else {
				newSts.Status.AllocatableBandwidth[key] = val
			}
		}

		if _, err := e.excs.DiskioV1alpha1().NodeDiskIOStatses(CRNameSpace).UpdateStatus(ctx, newSts, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to patch the NodeIOStatus: %v", err)
		}
		return nil
	})
}

// assume the lock is acquired
func (e *IODriver) calDiskAllocatable(diskId string) (*v1alpha1.IOBandwidth, error) {
	log.Println("now start to calDiskAllocatable")

	diskInfo, ok := e.diskInfos.Info[diskId]
	if !ok {
		return nil, fmt.Errorf("failed to get disk info for %s", diskId)
	}
	sizeRead := diskInfo.TotalRBPS
	sizeWrite := diskInfo.TotalWBPS
	sizeTotal := diskInfo.TotalBPS
	for _, podCrInfo := range e.podCrInfos {
		if podCrInfo.bw == nil {
			continue
		}
		sizeRead.Sub(podCrInfo.bw.Read)
		sizeWrite.Sub(podCrInfo.bw.Write)
		sizeTotal.Sub(podCrInfo.bw.Total)
	}

	return &v1alpha1.IOBandwidth{
		Total: sizeTotal,
		Write: sizeWrite,
		Read:  sizeRead,
	}, nil
}

// emulate
func (e *IODriver) getRealtimeDiskAllocable(diskId string) *v1alpha1.IOBandwidth {
	log.Println("now start to getRealtimeDiskAllocable")
	bw, err := e.calDiskAllocatable(diskId)
	if err != nil {
		return nil
	}
	if e.burstUp {
		bw.Total.Sub(MinDefaultTotalIOBW)
		bw.Write.Sub(MinDefaultIOBW)
		bw.Read.Sub(MinDefaultIOBW)
	}
	return bw
}

func (e *IODriver) CreateNodeDiskDeviceCR(ctx context.Context, c *v1alpha1.NodeDiskDevice) error {
	cur, err := e.excs.DiskioV1alpha1().NodeDiskDevices(CRNameSpace).Get(ctx, c.Name, metav1.GetOptions{})
	if err == nil {
		c.SetResourceVersion(cur.GetResourceVersion())
		// todo: retry
		_, err = e.excs.DiskioV1alpha1().NodeDiskDevices(CRNameSpace).Update(ctx, c, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else {
		_, err = e.excs.DiskioV1alpha1().NodeDiskDevices(CRNameSpace).Create(ctx, c, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}
