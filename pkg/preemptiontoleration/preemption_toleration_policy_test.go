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

package preemptiontoleration

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	schedulingv1 "k8s.io/api/scheduling/v1"
)

func TestParsePreemptionTolerationPolicyToleration(t *testing.T) {
	tests := []struct {
		name          string
		priorityClass *schedulingv1.PriorityClass
		wantErr       bool
		expected      *Policy
		errStr        string
	}{
		{
			name:          "PriorityClass with default values both on MinimumPreemptablePriority and TolerationSeconds",
			priorityClass: makePriorityClass(1, nil),
			expected: &Policy{
				MinimumPreemptablePriority: 2,
				TolerationSeconds:          0,
			},
		},
		{
			name: "PriorityClass with both values does return PriorityTolerationPolicy",
			priorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: "100",
				AnnotationKeyTolerationSeconds:          "10",
			}),
			expected: &Policy{
				MinimumPreemptablePriority: 100,
				TolerationSeconds:          10,
			},
		},
		{
			name: "PriorityClass with unparsable MinimumPreemptablePriority does raise error",
			priorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: "a",
			}),
			wantErr: true,
			errStr:  `strconv.ParseInt: parsing "a": invalid syntax`,
		},
		{
			name: "PriorityClass with unparsable TolerationSeconds does raise error",
			priorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyTolerationSeconds: "a",
			}),
			wantErr: true,
			errStr:  `strconv.ParseInt: parsing "a": invalid syntax`,
		},
		{
			name: "PriorityClass with negative TolerationSeconds does not raise error (But this will be able to tolerate forever)",
			priorityClass: makePriorityClass(1, map[string]string{
				AnnotationKeyMinimumPreemptablePriority: "100",
				AnnotationKeyTolerationSeconds:          "-1",
			}),
			expected: &Policy{
				MinimumPreemptablePriority: 100,
				TolerationSeconds:          -1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePreemptionTolerationPolicy(*tt.priorityClass)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error")
				} else {
					if diff := cmp.Diff(tt.errStr, err.Error()); diff != "" {
						t.Errorf("Unexpected error message (-expected, +got): %s", diff)
					}
				}
			} else {
				if err != nil {
					t.Errorf("Error is not expected: got %s", err.Error())
				}
				if diff := cmp.Diff(tt.expected, got); diff != "" {
					t.Errorf("Unexpected result (-expected, +got): %s", diff)
				}
			}
		})
	}
}
