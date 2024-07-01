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
	"os"
	"sync"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type IODriver struct {
	mu                 sync.RWMutex
	cs                 kubernetes.Interface
	excs               versioned.Interface
	nodeName           string
	diskInfos          *DiskInfos            // key:device id
	podCrInfos         map[string]*PodCrInfo // key:podUid
	observedGeneration *int64
	burstUp            bool
}

func NewIODriver(cfg Config) (*IODriver, error) {
	n, ok := os.LookupEnv("NodeName")
	if !ok {
		return nil, fmt.Errorf("init client failed: unable to get node name")
	}

	core, versioned, err := initClient(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("init client failed: %v", err)
	} else {
		log.Println("init client succeeded")
	}
	return &IODriver{
		cs:                 core,
		excs:               versioned,
		nodeName:           n,
		diskInfos:          &DiskInfos{},
		podCrInfos:         map[string]*PodCrInfo{},
		observedGeneration: nil,
		burstUp:            false,
	}, nil
}

func initClient(kubeconfig string) (kubernetes.Interface, versioned.Interface, error) {
	var err error
	var config *rest.Config
	_, err = os.Stat(kubeconfig)
	if err == nil {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, nil, err
	}

	coreclient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("core client init err: %v", err)
	}

	clientset, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("clientset init err: %v", err)
	}

	return coreclient, clientset, nil
}

func (c *IODriver) Run(ctx context.Context) {
	// get profile result and create DiskDevice CR
	info := GetDiskProfile()
	if err := c.ProcessProfile(ctx, info); err != nil {
		log.Fatal(err)
	}

	// watch NodeDiskIOStats and its handlers
	if err := c.WatchNodeDiskIOStats(ctx); err != nil {
		log.Fatal(err)
	}

	// update IO stats periodically to CR
	go wait.UntilWithContext(ctx, c.periodicUpdate, PeriodicUpdateInterval)

	<-ctx.Done()
	log.Println("IODriver stopped")
}

func (c *IODriver) periodicUpdate(ctx context.Context) {
	c.mu.RLock()
	bw := c.getRealtimeDiskAllocable(c.diskInfos.EmptyDir)
	c.mu.RUnlock()
	c.burstUp = !c.burstUp

	toUpdate := make(map[string]v1alpha1.IOBandwidth)
	toUpdate[c.diskInfos.EmptyDir] = *bw
	err := c.UpdateNodeDiskIOInfoStatus(ctx, toUpdate)
	if err != nil && !errors.IsNotFound(err) {
		log.Printf("failed to update node io stats: %v", err)
	}
}
