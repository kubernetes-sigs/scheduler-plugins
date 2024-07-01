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

package diskio

import (
	"testing"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	diskioresource "sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

func fakeResourceCache() diskioresource.ExtendedCache {
	ec := diskioresource.NewExtendedCache()
	rs := &Resource{
		nodeName: "node1",
		info: &NodeInfo{
			DefaultDevice: "dev1",
			DisksStatus: map[string]*DiskInfo{
				"dev1": {
					NormalizerName: "normalizer1",
					DiskName:       "disk1",
					Capacity: v1alpha1.IOBandwidth{
						Total: resource.MustParse("1000"),
						Read:  resource.MustParse("500"),
						Write: resource.MustParse("500"),
					},
					Allocatable: v1alpha1.IOBandwidth{
						Total: resource.MustParse("1000"),
						Read:  resource.MustParse("500"),
						Write: resource.MustParse("500"),
					},
				},
			},
		},
	}
	ec.SetExtendedResource("node1", rs)
	return ec
}
func TestHandle_AddCacheNodeInfo(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		node  string
		disks map[string]v1alpha1.DiskDevice
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "Add disk",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node2",
				disks: map[string]v1alpha1.DiskDevice{
					"dev1": {
						Name:   "disk1",
						Vendor: "vendor1",
						Model:  "model1",
						Capacity: v1alpha1.IOBandwidth{
							Total: resource.MustParse("1000"),
							Read:  resource.MustParse("500"),
							Write: resource.MustParse("500"),
						},
						Type: string(common.EmptyDir),
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			h.AddCacheNodeInfo(tt.args.node, tt.args.disks)
		})
	}
}

func TestHandle_DeleteCacheNodeInfo(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		nodeName string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "delete node",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				nodeName: "node1",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			if err := h.DeleteCacheNodeInfo(tt.args.nodeName); (err != nil) != tt.wantErr {
				t.Errorf("Handle.DeleteCacheNodeInfo() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHandle_UpdateCacheNodeStatus(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		nodeName string
		nodeIoBw v1alpha1.NodeDiskIOStatsStatus
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "update node resource",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				nodeName: "node1",
				nodeIoBw: v1alpha1.NodeDiskIOStatsStatus{
					AllocatableBandwidth: map[string]v1alpha1.IOBandwidth{
						"dev1": {
							Total: resource.MustParse("800"),
							Read:  resource.MustParse("400"),
							Write: resource.MustParse("400"),
						},
					},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			if err := h.UpdateCacheNodeStatus(tt.args.nodeName, tt.args.nodeIoBw); (err != nil) != tt.wantErr {
				t.Errorf("Handle.UpdateCacheNodeStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHandle_IsIORequired(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		annotations map[string]string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		{
			name: "has disk io annotation",
			args: args{
				annotations: map[string]string{
					common.DiskIOAnnotation: "",
				},
			},
			want: true,
		},
		{
			name: "has not disk io annotation",
			args: args{
				annotations: map[string]string{},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			if got := h.IsIORequired(tt.args.annotations); got != tt.want {
				t.Errorf("Handle.IsIORequired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandle_CanAdmitPod(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		nodeName string
		req      v1alpha1.IOBandwidth
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "can admit pod",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				nodeName: "node1",
				req: v1alpha1.IOBandwidth{
					Total: resource.MustParse("800"),
					Read:  resource.MustParse("400"),
					Write: resource.MustParse("400"),
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name: "can not admit pod",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				nodeName: "node1",
				req: v1alpha1.IOBandwidth{
					Total: resource.MustParse("2000"),
					Read:  resource.MustParse("400"),
					Write: resource.MustParse("400"),
				},
			},
			want:    false,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			got, err := h.CanAdmitPod(tt.args.nodeName, tt.args.req)
			if (err != nil) != tt.wantErr {
				t.Errorf("Handle.CanAdmitPod() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Handle.CanAdmitPod() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandle_GetDiskNormalizeModel(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		node string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    string
		wantErr bool
	}{
		{
			name: "get disk normalize model with success",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node1",
			},
			want:    "normalizer1",
			wantErr: false,
		},
		{
			name: "get disk normalize model fails",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node2",
			},
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			got, err := h.GetDiskNormalizeModel(tt.args.node)
			if (err != nil) != tt.wantErr {
				t.Errorf("Handle.GetDiskNormalizeModel() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Handle.GetDiskNormalizeModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandle_NodePressureRatio(t *testing.T) {
	type fields struct {
		HandleBase diskioresource.HandleBase
		client     kubernetes.Interface
	}
	type args struct {
		node    string
		request v1alpha1.IOBandwidth
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    float64
		wantErr bool
	}{
		{
			name: "node pressure ratio with success #1",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node1",
				request: v1alpha1.IOBandwidth{
					Total: resource.MustParse("800"),
					Read:  resource.MustParse("400"),
					Write: resource.MustParse("400"),
				},
			},
			want:    0.8,
			wantErr: false,
		},
		{
			name: "node pressure ratio with success #2",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node1",
				request: v1alpha1.IOBandwidth{
					Total: resource.MustParse("800"),
					Read:  resource.MustParse("400"),
					Write: resource.MustParse("100"),
				},
			},
			want:    0.5,
			wantErr: false,
		},
		{
			name: "node pressure ratio fails",
			fields: fields{
				HandleBase: diskioresource.HandleBase{
					EC: fakeResourceCache(),
				},
			},
			args: args{
				node: "node2",
				request: v1alpha1.IOBandwidth{
					Total: resource.MustParse("800"),
					Read:  resource.MustParse("400"),
					Write: resource.MustParse("100"),
				},
			},
			want:    0,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handle{
				HandleBase: tt.fields.HandleBase,
				client:     tt.fields.client,
			}
			got, err := h.NodePressureRatio(tt.args.node, tt.args.request)
			if (err != nil) != tt.wantErr {
				t.Errorf("Handle.NodePressureRatio() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Handle.NodePressureRatio() = %v, want %v", got, tt.want)
			}
		})
	}
}
