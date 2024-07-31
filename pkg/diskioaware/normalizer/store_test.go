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
	"testing"
)

var exec Normalize = func(in string) (string, error) {
	return in, nil
}

func TestNStore_Set(t *testing.T) {
	ns := NewnStore()
	type params struct {
		name string
		f    Normalize
	}
	tests := []struct {
		name     string
		p        *params
		expected bool
	}{
		{
			name: "Empty plugin name",
			p: &params{
				name: "",
				f:    nil,
			},
			expected: false,
		},
		{
			name: "Null exec func",
			p: &params{
				name: "p1",
				f:    nil,
			},
			expected: false,
		},
		{
			name: "Success",
			p: &params{
				name: "p1",
				f:    exec,
			},
			expected: true,
		},
		{
			name: "Duplicate plugin name",
			p: &params{
				name: "p1",
				f:    exec,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ns.Set(tt.p.name, tt.p.f)
			if (err == nil) != tt.expected {
				t.Errorf("case: %v failed got err=%v expected error=%v", tt.name, err, tt.expected)
			}
		})
	}
}

func TestNStore_Contains(t *testing.T) {
	ns := NewnStore()
	ns.Set("p1", exec)

	tests := []struct {
		name     string
		pName    string
		expected bool
	}{
		{
			name:     "Existing plugin",
			pName:    "p1",
			expected: true,
		},
		{
			name:     "Non-existing plugin",
			pName:    "p2",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ns.Contains(tt.pName)
			if got != tt.expected {
				t.Errorf("case: %v failed got=%v expected=%v", tt.name, got, tt.expected)
			}
		})
	}
}
