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

package numanode

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	canonicalPrefix = "node-"
)

// IDToName creates the canonical NUMA node name given the NUMA cell ID
func IDToName(numaID int) (string, error) {
	if numaID < 0 {
		return "", fmt.Errorf("invalid NUMA id range numaID: %d", numaID)
	}
	return canonicalPrefix + strconv.Itoa(numaID), nil
}

// NameToID retrieves the NUMA cell ID (integer) from the canonical
// NUMA node name (node-X, cell ID would be X).
func NameToID(node string) (int, error) {
	numaIDStr, ok := strings.CutPrefix(node, canonicalPrefix)
	if !ok {
		return -1, fmt.Errorf("invalid format for name %q: bad prefix", node)
	}
	numaID, err := strconv.Atoi(numaIDStr)
	if err != nil {
		return -1, fmt.Errorf("invalid format for name %q: %v", node, err)
	}
	if numaID < 0 {
		return -1, fmt.Errorf("invalid format for name %q: bad NUMA id: %d", node, numaID)
	}
	return numaID, nil
}
