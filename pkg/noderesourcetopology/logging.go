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

package noderesourcetopology

import (
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/stringify"
)

func logNumaNodes(desc, nodeName string, nodes NUMANodeList) {
	for _, numaNode := range nodes {
		numaLogKey := fmt.Sprintf("%s/node-%d", nodeName, numaNode.NUMAID)
		klog.V(6).InfoS(desc, stringify.ResourceListToLoggable(numaLogKey, numaNode.Resources)...)
	}
}

func logNRT(desc string, nrtObj *topologyv1alpha2.NodeResourceTopology) {
	if !klog.V(6).Enabled() {
		// avoid the expensive marshal operation
		return
	}

	ntrJson, err := json.MarshalIndent(nrtObj, "", " ")
	if err != nil {
		klog.V(6).ErrorS(err, "failed to marshal noderesourcetopology object")
		return
	}
	klog.V(6).Info(desc, "noderesourcetopology", string(ntrJson))
}
