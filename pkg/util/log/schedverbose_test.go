/*
Copyright 2020 The Kubernetes Authors.

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

package log

import (
	"flag"
	"fmt"
	"testing"

	"k8s.io/klog/v2"
)

type fakeObject map[string]string

func (fo fakeObject) GetName() string {
	return "" // unused by the code being exercised
}

func (fo fakeObject) GetNamespace() string {
	return "" // unused by the code being exercised
}

func (fo fakeObject) GetAnnotations() map[string]string {
	return fo
}

func TestV(t *testing.T) {
	type testCase struct {
		name        string
		flagValue   int
		paramValue  int
		annotations map[string]string
		expected    bool
	}

	testCases := []testCase{
		// sanity checks
		{
			name:       "nil object, too verbose",
			flagValue:  1,
			paramValue: 5,
			expected:   false,
		},
		{
			name:       "nil object, matches level",
			flagValue:  2,
			paramValue: 2,
			expected:   true,
		},
		{
			name:       "nil object, verbose enough",
			flagValue:  3,
			paramValue: 2,
			expected:   true,
		},
		{
			name:        "empty annotations, too verbose",
			flagValue:   1,
			paramValue:  5,
			annotations: map[string]string{},
			expected:    false,
		},
		{
			name:        "empty annotations, matches level",
			flagValue:   2,
			paramValue:  2,
			annotations: map[string]string{},
			expected:    true,
		},
		{
			name:        "empty annotations, verbose enough",
			flagValue:   3,
			paramValue:  2,
			annotations: map[string]string{},
			expected:    true,
		},
		{
			name:       "unrelated annotations, too verbose",
			flagValue:  1,
			paramValue: 5,
			annotations: map[string]string{
				"foo":         "",
				"sigs.k8s.io": "awesome",
			},
			expected: false,
		},
		{
			name:       "unrelated annotations, matches level",
			flagValue:  2,
			paramValue: 2,
			annotations: map[string]string{
				"foo":         "",
				"sigs.k8s.io": "awesome",
			},
			expected: true,
		},
		{
			name:       "unrelated annotations, verbose enough",
			flagValue:  3,
			paramValue: 2,
			annotations: map[string]string{
				"foo":         "",
				"sigs.k8s.io": "awesome",
			},
			expected: true,
		},
		// this demonstrates we can still silence logs
		{
			name:       "magic annotations, flag lower than UpgradedLogLevel",
			flagValue:  1,
			paramValue: 5,
			annotations: map[string]string{
				"foo":                        "",
				"sigs.k8s.io":                "awesome",
				ObjectSchedVerboseAnnotation: "",
			},
			expected: false,
		},
		// if the global -v setting matches our defaults, we emit the log
		{
			name:       "magic annotations, matching level",
			flagValue:  int(UpgradedLogLevel),
			paramValue: 5,
			annotations: map[string]string{
				"foo":                        "",
				"sigs.k8s.io":                "awesome",
				ObjectSchedVerboseAnnotation: "",
			},
			expected: true,
		},
		// overriding the global value because the object opted in
		{
			name:       "magic annotations, overriding verbose",
			flagValue:  3,
			paramValue: 5,
			annotations: map[string]string{
				"foo":                        "",
				"sigs.k8s.io":                "awesome",
				ObjectSchedVerboseAnnotation: "",
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			flags := &flag.FlagSet{}
			klog.InitFlags(flags)
			flags.Parse([]string{"-v", fmt.Sprintf("%d", tc.flagValue)})

			res := V(klog.Level(tc.paramValue), fakeObject(tc.annotations))
			if res.Enabled() != tc.expected {
				t.Errorf("flag=%d param=%d annotations=%v expected=%t got=%t", tc.flagValue, tc.paramValue, tc.annotations, tc.expected, res.Enabled())
			}
		})
	}
}
