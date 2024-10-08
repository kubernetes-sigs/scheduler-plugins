/*
Copyright 2021 The Kubernetes Authors.

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

package logging

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"
)

// well-known structured log keys
const (
	KeyLogID         string = "logID"
	KeyPod           string = "pod"
	KeyPodUID        string = "podUID"
	KeyNode          string = "node"
	KeyFlow          string = "flow"
	KeyContainer     string = "container"
	KeyContainerKind string = "kind"
	KeyGeneration    string = "generation"
)

const (
	FlowBegin string = "begin"
	FlowEnd   string = "end"
)

const (
	FlowCacheSync string = "resync"
)

const (
	KindContainerInit string = "init"
	KindContainerApp  string = "app"
)

const (
	SubsystemForeignPods string = "foreignpods"
	SubsystemNRTCache    string = "nrtcache"
)

func PodUID(pod *corev1.Pod) string {
	if pod == nil {
		return "<nil>"
	}
	if val := reflect.ValueOf(pod); val.Kind() == reflect.Ptr && val.IsNil() {
		return "<nil>"
	}
	return string(pod.GetUID())
}
