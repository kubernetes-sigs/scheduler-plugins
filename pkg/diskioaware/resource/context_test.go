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

package resource

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

func TestResourceIOContext_AddPod(t *testing.T) {
	type fields struct {
		VClient     versioned.Interface
		Reservedpod map[string][]string
		PodRequests map[string]v1alpha1.IOBandwidth
		NsWhiteList []string
		Queue       workqueue.RateLimitingInterface
	}
	type args struct {
		pod      *corev1.Pod
		nodeName string
		bw       v1alpha1.IOBandwidth
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "Add pod to ResourceIOContext with success",
			fields: fields{
				Reservedpod: map[string][]string{
					"node1": {
						"pod2-456",
					},
				},
				PodRequests: map[string]v1alpha1.IOBandwidth{},
				Queue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test"),
			},
			args: args{
				pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "123",
					},
				},
				nodeName: "node1",
				bw: v1alpha1.IOBandwidth{
					Total: resource.MustParse("1000"),
					Read:  resource.MustParse("500"),
					Write: resource.MustParse("500"),
				},
			},
			wantErr: false,
		},
		{
			name: "Add pod to ResourceIOContext fails",
			fields: fields{
				Reservedpod: map[string][]string{},
				PodRequests: map[string]v1alpha1.IOBandwidth{},
				Queue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test"),
			},
			args: args{
				pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "123",
					},
				},
				nodeName: "node1",
				bw: v1alpha1.IOBandwidth{
					Total: resource.MustParse("1000"),
					Read:  resource.MustParse("500"),
					Write: resource.MustParse("500"),
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ResourceIOContext{
				VClient:     tt.fields.VClient,
				Reservedpod: tt.fields.Reservedpod,
				PodRequests: tt.fields.PodRequests,
				NsWhiteList: tt.fields.NsWhiteList,
				Queue:       tt.fields.Queue,
			}
			if err := c.AddPod(tt.args.pod, tt.args.nodeName, tt.args.bw); (err != nil) != tt.wantErr {
				t.Errorf("ResourceIOContext.AddPod() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResourceIOContext_RemovePod(t *testing.T) {
	type fields struct {
		VClient     versioned.Interface
		Reservedpod map[string][]string
		PodRequests map[string]v1alpha1.IOBandwidth
		NsWhiteList []string
		Queue       workqueue.RateLimitingInterface
	}
	type args struct {
		pod      *corev1.Pod
		nodeName string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "Remove pod from ResourceIOContext with success",
			fields: fields{
				Reservedpod: map[string][]string{
					"node1": {
						"pod1-123",
						"pod2-456",
					},
				},
				PodRequests: map[string]v1alpha1.IOBandwidth{
					"123": {
						Total: resource.MustParse("1000"),
						Read:  resource.MustParse("500"),
						Write: resource.MustParse("500"),
					},
					"456": {
						Total: resource.MustParse("1000"),
						Read:  resource.MustParse("500"),
						Write: resource.MustParse("500"),
					},
				},
				Queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test"),
			},
			args: args{
				pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "123",
					},
				},
				nodeName: "node1",
			},
			wantErr: false,
		},
		{
			name: "Remove pod to ResourceIOContext fails",
			fields: fields{
				Reservedpod: map[string][]string{},
				PodRequests: map[string]v1alpha1.IOBandwidth{},
				Queue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test"),
			},
			args: args{
				pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "123",
					},
				},
				nodeName: "node1",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ResourceIOContext{
				VClient:     tt.fields.VClient,
				Reservedpod: tt.fields.Reservedpod,
				PodRequests: tt.fields.PodRequests,
				NsWhiteList: tt.fields.NsWhiteList,
				Queue:       tt.fields.Queue,
			}
			if err := c.RemovePod(tt.args.pod, tt.args.nodeName); (err != nil) != tt.wantErr {
				t.Errorf("ResourceIOContext.RemovePod() error = %v, wantErr %v", err, tt.wantErr)
			}
			klog.Info(c.Reservedpod)
			klog.Info(c.PodRequests)
		})
	}
}

func TestResourceIOContext_InNamespaceWhiteList(t *testing.T) {
	type fields struct {
		NsWhiteList []string
	}
	type args struct {
		ns string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		{
			name: "namespace exists",
			fields: fields{
				NsWhiteList: []string{
					"ns1", "ns2",
				},
			},
			args: args{
				"ns1",
			},
			want: true,
		},
		{
			name: "namespace does not exist",
			fields: fields{
				NsWhiteList: []string{
					"ns1", "ns2",
				},
			},
			args: args{
				"ns3",
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ResourceIOContext{
				VClient:     nil,
				Reservedpod: nil,
				PodRequests: nil,
				NsWhiteList: tt.fields.NsWhiteList,
				Queue:       nil,
			}
			if got := c.InNamespaceWhiteList(tt.args.ns); got != tt.want {
				t.Errorf("ResourceIOContext.InNamespaceWhiteList() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResourceIOContext_GetPodRequest(t *testing.T) {
	type fields struct {
		PodRequests map[string]v1alpha1.IOBandwidth
	}
	type args struct {
		pod string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    v1alpha1.IOBandwidth
		wantErr bool
	}{
		{
			name: "pod exists",
			fields: fields{
				PodRequests: map[string]v1alpha1.IOBandwidth{
					"pod1": {},
				},
			},
			args: args{
				pod: "pod1",
			},
			want:    v1alpha1.IOBandwidth{},
			wantErr: false,
		},
		{
			name: "pod does not exist",
			fields: fields{
				PodRequests: map[string]v1alpha1.IOBandwidth{
					"pod1": {},
				},
			},
			args: args{
				pod: "pod2",
			},
			want:    v1alpha1.IOBandwidth{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ResourceIOContext{
				VClient:     nil,
				Reservedpod: nil,
				PodRequests: tt.fields.PodRequests,
				NsWhiteList: nil,
				Queue:       nil,
			}
			got, err := c.GetPodRequest(tt.args.pod)
			if (err != nil) != tt.wantErr {
				t.Errorf("ResourceIOContext.GetPodRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResourceIOContext.GetPodRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResourceIOContext_RemoveNode(t *testing.T) {
	type fields struct {
		Reservedpod map[string][]string
	}
	type args struct {
		node string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "remove existing node",
			fields: fields{
				Reservedpod: map[string][]string{
					"pod1": {},
				},
			},
			args: args{
				node: "pod1",
			},
		},
		{
			name: "remove non-existing node",
			fields: fields{
				Reservedpod: map[string][]string{
					"pod1": {},
				},
			},
			args: args{
				node: "pod2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ResourceIOContext{
				VClient:     nil,
				Reservedpod: tt.fields.Reservedpod,
				PodRequests: nil,
				NsWhiteList: nil,
				Queue:       nil,
			}
			c.RemoveNode(tt.args.node)
			if _, err := c.GetReservedPods(tt.args.node); err == nil {
				t.Errorf("ResourceIOContext.RemoveNode() failed, node %s still exists", tt.args.node)
			}
		})
	}
}

func TestNewContext(t *testing.T) {
	ctx := context.Background()
	fw, err := runtime.NewFramework(ctx,
		nil,
		nil,
		runtime.WithKubeConfig(&rest.Config{}),
		runtime.WithClientSet(kubernetes.NewForConfigOrDie(&rest.Config{})),
	)
	if err != nil {
		t.Fatalf("failed to create framework: %v", err)
	}
	type args struct {
		rl workqueue.RateLimiter
		wl []string
		h  framework.Handle
	}
	tests := []struct {
		name    string
		args    args
		want    *ResourceIOContext
		wantErr bool
	}{
		{
			name: "rate limiter is nil",
			args: args{
				rl: nil,
				wl: []string{"default"},
				h:  nil,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "success",
			args: args{
				rl: workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Second),
				wl: []string{"default"},
				h:  fw,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewContext(tt.args.rl, tt.args.wl, tt.args.h)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewContext() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResourceIOContext_RunWorkerQueue(t *testing.T) {
	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Second)
	type fields struct {
		Queue workqueue.RateLimitingInterface
	}
	type args struct {
		item *SyncContext
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "success",
			fields: fields{
				Queue: workqueue.NewNamedRateLimitingQueue(rateLimiter, "test"),
			},
			args: args{
				item: &SyncContext{
					Node: "nodeA",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			c := &ResourceIOContext{
				Queue: tt.fields.Queue,
			}
			go c.RunWorkerQueue(ctx)
			c.Queue.Add(tt.args.item)
			<-ctx.Done()
		})
	}
}
