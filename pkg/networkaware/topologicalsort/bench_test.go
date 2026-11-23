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

package topologicalsort

import (
	"context"
	"fmt"
	"testing"

	agv1alpha1 "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// Benchmark dimensions
// ---------------------------------------------------------------------------

// benchTSPodCounts are the pod-queue sizes used in all TS benchmark tables.
// Each iteration runs N parallel Less() comparisons.
var benchTSPodCounts = []int{10, 100, 500, 1000}

// ---------------------------------------------------------------------------
// BenchmarkTopologicalSort_Less
// ---------------------------------------------------------------------------

// BenchmarkTopologicalSort_Less measures the cost of N parallel Less() comparisons
// where N is the pod-queue depth. Each individual Less() is O(log k) where k is the
// number of workloads in the AppGroup (11 for onlineboutique).
func BenchmarkTopologicalSort_Less(b *testing.B) {
	for _, n := range benchTSPodCounts {
		n := n
		b.Run(fmt.Sprintf("%dpods", n), func(b *testing.B) {
			ctx := context.TODO()

			s := clientgoscheme.Scheme
			utilruntime.Must(agv1alpha1.AddToScheme(s))

			ag := GetAppGroupCROnlineBoutique()
			client := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(ag).
				WithStatusSubresource(&agv1alpha1.AppGroup{}).
				Build()

			ts := &TopologicalSort{
				Client:     client,
				namespaces: []string{"default"},
			}

			selectors := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "p11"}
			podNames := []string{
				"p1-deployment", "p2-deployment", "p3-deployment", "p4-deployment",
				"p5-deployment", "p6-deployment", "p7-deployment", "p8-deployment",
				"p9-deployment", "p10-deployment", "p11-deployment",
			}

			pInfo1 := getPodInfos(b, n, "onlineboutique", selectors, podNames)
			pInfo2 := getPodInfos(b, n, "onlineboutique", selectors, podNames)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				sorting := func(i int) {
					_ = ts.Less(pInfo1[i], pInfo2[i])
				}
				Until(ctx, len(pInfo1), sorting)
			}
		})
	}
}
