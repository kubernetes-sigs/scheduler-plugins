/*
Copyright 2022 The Kubernetes Authors.

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

package stringify

import (
	"sort"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"

	corev1 "k8s.io/api/core/v1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2/helper/numanode"
)

func ResourceListToLoggable(resources corev1.ResourceList) []interface{} {
	return ResourceListToLoggableWithValues([]interface{}{}, resources)
}

func ResourceListToLoggableWithValues(items []interface{}, resources corev1.ResourceList) []interface{} {
	resNames := []string{}
	for resName := range resources {
		resNames = append(resNames, string(resName))
	}
	sort.Strings(resNames)

	for _, resName := range resNames {
		qty := resources[corev1.ResourceName(resName)]
		items = append(items, resName)
		resVal, _ := qty.AsInt64()
		if needsHumanization(resName) {
			items = append(items, humanize.IBytes(uint64(resVal)))
		} else {
			items = append(items, strconv.FormatInt(resVal, 10))
		}
	}
	return items
}

func ResourceList(resources corev1.ResourceList) string {
	resNames := []string{}
	for resName := range resources {
		resNames = append(resNames, string(resName))
	}
	sort.Strings(resNames)

	resItems := []string{}
	for _, resName := range resNames {
		qty := resources[corev1.ResourceName(resName)]
		resVal, _ := qty.AsInt64()
		if needsHumanization(resName) {
			resItems = append(resItems, resName+"="+humanize.IBytes(uint64(resVal)))
		} else {
			resItems = append(resItems, resName+"="+strconv.FormatInt(resVal, 10))
		}
	}
	return strings.Join(resItems, ",")
}

func NodeResourceTopologyResources(nrtObj *topologyv1alpha2.NodeResourceTopology) string {
	zones := []string{}
	for _, zoneInfo := range nrtObj.Zones {
		numaItems := []interface{}{"numaCell"}
		if numaID, err := numanode.NameToID(zoneInfo.Name); err == nil {
			numaItems = append(numaItems, numaID)
		} else {
			numaItems = append(numaItems, zoneInfo.Name)
		}
		zones = append(zones, zoneInfo.Name+"=<"+nrtResourceInfoListToString(zoneInfo.Resources)+">")
	}
	return nrtObj.Name + "={" + strings.Join(zones, ",") + "}"
}

func nrtResourceInfoListToString(resInfoList []topologyv1alpha2.ResourceInfo) string {
	items := []string{}
	for _, resInfo := range resInfoList {
		items = append(items, nrtResourceInfo(resInfo))
	}
	return strings.Join(items, ",")
}

func nrtResourceInfo(resInfo topologyv1alpha2.ResourceInfo) string {
	capVal, _ := resInfo.Capacity.AsInt64()
	allocVal, _ := resInfo.Allocatable.AsInt64()
	availVal, _ := resInfo.Available.AsInt64()
	if !needsHumanization(resInfo.Name) {
		return resInfo.Name + "=" + strconv.FormatInt(capVal, 10) + "/" + strconv.FormatInt(allocVal, 10) + "/" + strconv.FormatInt(availVal, 10)
	}
	return resInfo.Name + "=" + humanize.IBytes(uint64(capVal)) + "/" + humanize.IBytes(uint64(allocVal)) + "/" + humanize.IBytes(uint64(availVal))
}

func needsHumanization(rn string) bool {
	resName := corev1.ResourceName(rn)
	// memory-related resources may be expressed in KiB/Bytes, which makes
	// for long numbers, harder to read and compare. To make it easier for
	// the reader, we express them in a more compact form using go-humanize.
	if resName == corev1.ResourceMemory {
		return true
	}
	if resName == corev1.ResourceStorage || resName == corev1.ResourceEphemeralStorage {
		return true
	}
	return v1helper.IsHugePageResourceName(resName)
}
