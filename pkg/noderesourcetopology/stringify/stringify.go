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
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"

	corev1 "k8s.io/api/core/v1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

func ResourceListToLoggable(logID string, resources corev1.ResourceList) []interface{} {
	items := []interface{}{"logID", logID}

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
		zones = append(zones, zoneInfo.Name+"=<"+nrtResourceInfoListToString(zoneInfo.Resources)+">")
	}
	return nrtObj.Name + "={" + strings.Join(zones, ",") + "}"
}

func nrtResourceInfoListToString(resInfoList []topologyv1alpha2.ResourceInfo) string {
	items := []string{}
	for _, resInfo := range resInfoList {
		items = append(items, fmt.Sprintf("%s=%s/%s/%s", resInfo.Name, resInfo.Capacity.String(), resInfo.Allocatable.String(), resInfo.Available.String()))
	}
	return strings.Join(items, ",")
}
func needsHumanization(resName string) bool {
	// memory-related resources may be expressed in KiB/Bytes, which makes
	// for long numbers, harder to read and compare. To make it easier for
	// the reader, we express them in a more compact form using go-humanize.
	return resName == string(corev1.ResourceMemory) || v1helper.IsHugePageResourceName(corev1.ResourceName(resName))
}
