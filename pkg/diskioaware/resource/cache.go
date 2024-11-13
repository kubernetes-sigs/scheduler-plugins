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

package resource

import (
	"k8s.io/klog/v2"
)

type ExtendedCache interface {
	SetExtendedResource(nodeName string, val ExtendedResource)
	GetExtendedResource(nodeName string) ExtendedResource
	DeleteExtendedResource(nodeName string)
	PrintCacheInfo()
}

type ResourceCache struct {
	Resources map[string]ExtendedResource
}

func NewExtendedCache() ExtendedCache {
	c := &ResourceCache{
		Resources: make(map[string]ExtendedResource),
	}

	return c
}

func (cache *ResourceCache) SetExtendedResource(nodeName string, val ExtendedResource) {
	cache.Resources[nodeName] = val
}

func (cache *ResourceCache) GetExtendedResource(nodeName string) ExtendedResource {
	val, ok := cache.Resources[nodeName]
	if !ok {
		return nil
	}
	return val
}

func (cache *ResourceCache) DeleteExtendedResource(nodeName string) {
	delete(cache.Resources, nodeName)
}

func (cache *ResourceCache) PrintCacheInfo() {
	for node, er := range cache.Resources {
		klog.V(2).Infof("node %s 's device info has been updated", node)
		er.PrintInfo()
	}
}
