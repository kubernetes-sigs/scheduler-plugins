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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	NodeDiskDeviceCRSuffix string        = "nodediskdevice"
	NodeDiskIOInfoCRSuffix string        = "nodediskiostats"
	CRNameSpace            string        = "ioi-system"
	DiskIOAnnotation       string        = "blockio.kubernetes.io/resources"
	NodeIOStatusCR         string        = "NodeDiskIOStats"
	APIVersion             string        = "ioi.intel.com/v1"
	PeriodicUpdateInterval time.Duration = 5 * time.Second
	Mi                     float64       = 1024 * 1024
	EmptyDir               DeviceType    = "emptyDir"
	Others                 DeviceType    = "others"
)

var (
	MinDefaultIOBW      resource.Quantity = resource.MustParse("5Mi")
	MinDefaultTotalIOBW resource.Quantity = resource.MustParse("10Mi")

	UpdateBackoff = wait.Backoff{
		Steps:    3,
		Duration: 100 * time.Millisecond, // 0.1s
		Jitter:   1.0,
	}
)

func GetCRName(n string, suff string) string {
	return fmt.Sprintf("%v-%v", n, suff)
}

func GetSliceIdx(target string, s []string) int {
	for i, v := range s {
		if v == target {
			return i
		}
	}
	return -1
}
