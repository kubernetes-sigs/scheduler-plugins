// hook_postfilter_test.go
package mypriorityoptimizer

import (
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

// We only test the fast-path where PerPod@PostFilter is NOT enabled:
// PostFilter should return Unschedulable with a "no nomination" message.
func TestPostFilter_NoPerPod_NoNomination(t *testing.T) {

	pl := &SharedState{}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p1",
			Namespace: "default",
		},
	}

	res, st := pl.PostFilter(context.Background(), nil, pod, nil)
	if res != nil {
		t.Fatalf("PostFilter() result = %#v, want nil when no nomination", res)
	}
	if st == nil {
		t.Fatalf("PostFilter() returned nil status")
	}
	if st.Code() != fwk.Unschedulable {
		t.Fatalf("PostFilter() code = %v, want %v", st.Code(), fwk.Unschedulable)
	}
	if msg := st.Message(); !strings.Contains(msg, "PostFilter: no nomination") {
		t.Fatalf("PostFilter() message = %q, want to contain %q", msg, "PostFilter: no nomination")
	}
}
