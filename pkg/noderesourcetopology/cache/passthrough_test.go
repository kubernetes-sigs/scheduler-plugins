/*
Copyright 2023 The Kubernetes Authors.

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

package cache

import (
	"testing"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPassthroughGetCachedNRTCopy(t *testing.T) {
	testNodeName := "worker-node-1"
	nrt := makeTestNRT(testNodeName)

	testCases := []testCaseGetCachedNRTCopy{
		{
			name: "data present with foreign pods",
			nodeTopologies: []*topologyv1alpha2.NodeResourceTopology{
				nrt,
			},
			nodeName:       testNodeName,
			hasForeignPods: true,
			expectedNRT:    nrt,
			expectedOK:     true,
		},
	}

	checkGetCachedNRTCopy(
		t,
		func(client ctrlclient.WithWatch, _ podlisterv1.PodLister) (Interface, error) {
			return NewPassthrough(klog.Background(), client), nil
		},
		testCases...,
	)
}
