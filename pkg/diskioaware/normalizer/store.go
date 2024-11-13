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

import "fmt"

type nStore struct {
	executer map[string]Normalize
}

func NewnStore() *nStore {
	return &nStore{
		executer: make(map[string]Normalize),
	}
}

func (ns *nStore) Set(name string, n Normalize) error {
	if len(name) == 0 {
		return fmt.Errorf("normalizer name cannot be empty")
	}
	if n == nil {
		return fmt.Errorf("normalizer func cannot be empty")
	}
	ns.executer[name] = n
	return nil
}

func (ns *nStore) Contains(name string) bool {
	_, ok := ns.executer[name]
	return ok
}

func (ns *nStore) Delete(name string) {
	delete(ns.executer, name)
}

func (ns *nStore) Get(name string) (Normalize, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("normalizer name cannot be empty")
	}
	n, ok := ns.executer[name]
	if !ok {
		return nil, fmt.Errorf("normalizer %s not found", name)
	}
	return n, nil
}
