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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func getBytes(fileName string) ([]byte, error) {
	if len(fileName) == 0 {
		return nil, fmt.Errorf("file name is empty")
	}
	p, err := os.ReadFile(fmt.Sprintf("../sampleplugin/%s/%v.so", fileName, fileName))
	if err != nil {
		return nil, fmt.Errorf("failed to %s.so: %v", fileName, err)
	}
	return p, nil
}

func TestNormalizerManager_LoadPlugin(t *testing.T) {
	p, err := getBytes("foo")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(p)
	}))
	defer ts.Close()

	tests := []struct {
		name   string
		plugin *PlConfig
		errMsg string
	}{
		{
			name:   "Empty plugin list",
			plugin: &PlConfig{},
			errMsg: "invalid plugin configuration",
		},
		{
			name:   "Successful plugin http loading",
			plugin: &PlConfig{Vendor: "Intel", Model: "P1111", URL: ts.URL},
			errMsg: "",
		},
		{
			name:   "Successful plugin http loading",
			plugin: &PlConfig{Vendor: "Intel", Model: "P1111", URL: "file:///../sampleplugin/foo/foo.so"},
			errMsg: "",
		},
	}

	path, err := os.MkdirTemp("/tmp", "normalizer")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	nm := NewNormalizerManager(path, 2)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := nm.LoadPlugin(context.Background(), *tt.plugin)
			if err != nil && len(tt.errMsg) > 0 && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("case: %v failed expected error=%v, got=%v", tt.name, tt.errMsg, err)
			}

			if err == nil {
				key := fmt.Sprintf("%s-%s", tt.plugin.Vendor, tt.plugin.Model)
				if _, err = nm.GetNormalizer(key); err != nil {
					t.Errorf("case: %v failed key %v was not set", tt.name, key)
				}
			}
		})
	}
}

func TestNormalizerManager_UnloadPlugin(t *testing.T) {
	s := NewnStore()
	s.Set("plugin1", func(in string) (string, error) {
		return in, nil
	})
	manager := &NormalizerManager{
		store:  s,
		loader: &PluginLoader{},
	}

	tests := []struct {
		name     string
		pName    string
		expected bool
	}{
		{
			name:     "Successful plugin unloading",
			pName:    "plugin1",
			expected: true,
		},
		{
			name:     "Failed plugin unloading",
			pName:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := manager.UnloadPlugin(tt.pName)
			if (err == nil) != tt.expected {
				t.Errorf("case: %v failed expected error=%v", tt.name, tt.expected)
			}
		})
	}
}

func TestNormalizerManager_GetNormalizer(t *testing.T) {
	s := NewnStore()
	s.Set("plugin1", func(in string) (string, error) {
		return in, nil
	})
	manager := &NormalizerManager{
		store:  s,
		loader: &PluginLoader{},
	}

	tests := []struct {
		name     string
		pName    string
		expected bool
	}{
		{
			name:     "Existing plugin",
			pName:    "plugin1",
			expected: true,
		},
		{
			name:     "Non-existing plugin",
			pName:    "plugin2",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manager.GetNormalizer(tt.pName)
			if (err == nil) != tt.expected {
				t.Errorf("case: %v failed got err=%v expected error=%v", tt.name, err, tt.expected)
			}
		})
	}
}
