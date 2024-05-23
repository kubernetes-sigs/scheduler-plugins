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
	"bytes"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResourceListToLoggable(t *testing.T) {
	tests := []struct {
		name      string
		logID     string
		resources corev1.ResourceList
		expected  string
	}{
		{
			name:      "empty",
			logID:     "",
			resources: corev1.ResourceList{},
			expected:  ` logID=""`,
		},
		{
			name:      "only logID",
			logID:     "TEST1",
			resources: corev1.ResourceList{},
			expected:  ` logID="TEST1"`,
		},
		{
			name:  "only CPUs",
			logID: "TEST1",
			resources: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("16"),
			},
			expected: ` logID="TEST1" cpu="16"`,
		},
		{
			name:  "only Memory",
			logID: "TEST2",
			resources: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			expected: ` logID="TEST2" memory="16 GiB"`,
		},
		{
			name:  "CPUs and Memory, no logID",
			logID: "",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("24"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			expected: ` logID="" cpu="24" memory="16 GiB"`,
		},
		{
			name:  "CPUs and Memory",
			logID: "TEST3",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("24"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			expected: ` logID="TEST3" cpu="24" memory="16 GiB"`,
		},
		{
			name:  "CPUs, Memory, hugepages-2Mi",
			logID: "TEST4",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:                   resource.MustParse("24"),
				corev1.ResourceMemory:                resource.MustParse("16Gi"),
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi"),
			},
			expected: ` logID="TEST4" cpu="24" hugepages-2Mi="1.0 GiB" memory="16 GiB"`,
		},
		{
			name:  "CPUs, Memory, hugepages-2Mi, hugepages-1Gi",
			logID: "TEST4",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:                   resource.MustParse("24"),
				corev1.ResourceMemory:                resource.MustParse("16Gi"),
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi"),
				corev1.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
			},
			expected: ` logID="TEST4" cpu="24" hugepages-1Gi="2.0 GiB" hugepages-2Mi="1.0 GiB" memory="16 GiB"`,
		},
		{
			name:  "CPUs, Memory, hugepages-2Mi, devices",
			logID: "TEST4",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:                           resource.MustParse("24"),
				corev1.ResourceMemory:                        resource.MustParse("16Gi"),
				corev1.ResourceName("hugepages-2Mi"):         resource.MustParse("1Gi"),
				corev1.ResourceName("example.com/netdevice"): resource.MustParse("16"),
				corev1.ResourceName("awesome.net/gpu"):       resource.MustParse("4"),
			},
			expected: ` logID="TEST4" awesome.net/gpu="4" cpu="24" example.com/netdevice="16" hugepages-2Mi="1.0 GiB" memory="16 GiB"`,
		},
		{
			name:  "CPUs, Memory, EphemeralStorage",
			logID: "TEST5",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("24"),
				corev1.ResourceMemory:           resource.MustParse("16Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
			},
			expected: ` logID="TEST5" cpu="24" ephemeral-storage="4.0 GiB" memory="16 GiB"`,
		},
		{
			name:  "CPUs, Memory, EphemeralStorage, hugepages-1Gi",
			logID: "TEST6",
			resources: corev1.ResourceList{
				corev1.ResourceCPU:                   resource.MustParse("24"),
				corev1.ResourceMemory:                resource.MustParse("16Gi"),
				corev1.ResourceName("hugepages-1Gi"): resource.MustParse("4Gi"),
				corev1.ResourceEphemeralStorage:      resource.MustParse("6Gi"),
			},
			expected: ` logID="TEST6" cpu="24" ephemeral-storage="6.0 GiB" hugepages-1Gi="4.0 GiB" memory="16 GiB"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			keysAndValues := ResourceListToLoggableWithValues([]interface{}{"logID", tt.logID}, tt.resources)
			kvListFormat(&buf, keysAndValues...)
			got := buf.String()
			if got != tt.expected {
				t.Errorf("got=%q expected=%q", got, tt.expected)
			}
		})
	}
}

// taken from klog

const missingValue = "(MISSING)"

func kvListFormat(b *bytes.Buffer, keysAndValues ...interface{}) {
	for i := 0; i < len(keysAndValues); i += 2 {
		var v interface{}
		k := keysAndValues[i]
		if i+1 < len(keysAndValues) {
			v = keysAndValues[i+1]
		} else {
			v = missingValue
		}
		b.WriteByte(' ')

		switch v.(type) {
		case string, error:
			b.WriteString(fmt.Sprintf("%s=%q", k, v))
		case []byte:
			b.WriteString(fmt.Sprintf("%s=%+q", k, v))
		default:
			if _, ok := v.(fmt.Stringer); ok {
				b.WriteString(fmt.Sprintf("%s=%q", k, v))
			} else {
				b.WriteString(fmt.Sprintf("%s=%+v", k, v))
			}
		}
	}
}
