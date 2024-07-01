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

package normalizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	resyncDuration = 90 * time.Second
)

type PlList []PlConfig

type PlConfig struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	URL    string `json:"url"`
}

type NormalizerManager struct {
	sync.RWMutex
	store      *nStore
	loader     *PluginLoader
	queue      workqueue.RateLimitingInterface
	maxRetries int
}

func NewNormalizerManager(base string, m int) *NormalizerManager {
	return &NormalizerManager{
		store:      NewnStore(),
		loader:     NewPluginLoader(base),
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "normaliazer-manager"),
		maxRetries: m,
	}
}

func (pm *NormalizerManager) Run(ctx context.Context, diskModelConfig string, workers int) {
	defer utilruntime.HandleCrash()
	defer pm.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.V(5).Info("Starting normalizer manager")
	defer logger.V(5).Info("Shutting down normalizer manager")

	var periodJob = func(context.Context) {
		data, err := os.ReadFile(diskModelConfig)
		if err != nil {
			klog.Errorf("failed to load disk model config: %v", err)
		}
		pls := &PlList{}
		if err := json.Unmarshal(data, pls); err != nil {
			klog.Errorf("failed to deserialize disk model config: %v", err)
		}
		// enqueue not existing plugins to load
		for _, p := range *pls {
			key := fmt.Sprintf("%s-%s", p.Vendor, p.Model)
			if pm.store.Contains(key) {
				continue
			}
			pm.queue.Add(p)
		}
	}
	go wait.UntilWithContext(ctx, periodJob, resyncDuration)

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, pm.runWorker, time.Second)
	}

	<-ctx.Done()
}

func (pm *NormalizerManager) runWorker(ctx context.Context) {
	for pm.processNextWorkItem(ctx) {
	}
}

func (pm *NormalizerManager) processNextWorkItem(ctx context.Context) bool {
	key, quit := pm.queue.Get()
	if quit {
		return false
	}
	defer pm.queue.Done(key)

	err := pm.LoadPlugin(ctx, key.(PlConfig))
	if err == nil {
		pm.queue.Forget(key)
	} else if pm.queue.NumRequeues(key) < pm.maxRetries {
		pm.queue.AddRateLimited(key)
	} else {
		utilruntime.HandleError(fmt.Errorf("load plugin %q failed: %v", key, err))
		pm.queue.Forget(key)
	}
	return true
}

// LoadPlugin implements the interface method
func (pm *NormalizerManager) LoadPlugin(ctx context.Context, p PlConfig) error {
	// use Vendor+Model as key,
	key := fmt.Sprintf("%s-%s", p.Vendor, p.Model)
	klog.V(2).Infof("Loading plugin %s", key)
	norm, err := pm.loader.LoadPlugin(ctx, p)
	if err != nil {
		return err
	}

	// normalizer functions as value
	pm.store.Set(key, norm)
	klog.V(2).Infof("Plugin %s is loaded", key)
	return nil
}

// UnloadPlugin implements the interface method
func (pm *NormalizerManager) UnloadPlugin(name string) error {
	if len(name) == 0 {
		return errors.New("plugin name cannot be empty")
	}

	pm.Lock()
	defer pm.Unlock()
	pm.store.Delete(name)

	return nil
}

// GetPlugin implements the interface method
func (pm *NormalizerManager) GetNormalizer(name string) (Normalize, error) {
	pm.RLock()
	defer pm.RUnlock()
	exec, err := pm.store.Get(name)
	if err != nil {
		return nil, err
	}

	return exec, nil
}
