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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	diskioresource "sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

func TestResource_AddPod(t *testing.T) {
	type fields struct {
		nodeName string
		info     *NodeInfo
		ch       diskioresource.CacheHandle
	}
	type args struct {
		pod     *v1.Pod
		request v1alpha1.IOBandwidth
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "Test AddPod with success",
			fields: fields{
				nodeName: "node1",
				info: &NodeInfo{
					DefaultDevice: "dev1",
					DisksStatus: map[string]*DiskInfo{
						"dev1": {
							NormalizerName: "normalizer1",
							DiskName:       "dev1Name",
							Capacity: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
							Allocatable: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
						},
					},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID: "123",
					},
				},
				request: v1alpha1.IOBandwidth{
					Read:  resource.MustParse("200"),
					Write: resource.MustParse("200"),
					Total: resource.MustParse("400"),
				},
			},
			wantErr: false,
		},
		{
			name: "Test AddPod fails",
			fields: fields{
				nodeName: "node1",
				info: &NodeInfo{
					DefaultDevice: "dev1",
					DisksStatus:   map[string]*DiskInfo{},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID: "123",
					},
				},
				request: v1alpha1.IOBandwidth{
					Read:  resource.MustParse("200"),
					Write: resource.MustParse("200"),
					Total: resource.MustParse("400"),
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := &Resource{
				nodeName: tt.fields.nodeName,
				info:     tt.fields.info,
				ch:       tt.fields.ch,
			}
			if err := ps.AddPod(tt.args.pod, tt.args.request); (err != nil) != tt.wantErr {
				t.Errorf("Resource.AddPod() error = %v, wantErr %v", err, tt.wantErr)
			}
			// klog.Info(ps.info.DisksStatus["dev1"].Allocatable)
		})
	}
}

func TestResource_RemovePod(t *testing.T) {
	type fields struct {
		nodeName string
		info     *NodeInfo
		ch       diskioresource.CacheHandle
	}
	type args struct {
		pod *v1.Pod
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "Test RemovePod with success",
			fields: fields{
				nodeName: "node1",
				info: &NodeInfo{
					DefaultDevice: "dev1",
					DisksStatus: map[string]*DiskInfo{
						"dev1": {
							NormalizerName: "normalizer1",
							DiskName:       "dev1Name",
							Capacity: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
							Allocatable: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
						},
					},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID: "123",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Test RemovePod fails",
			fields: fields{
				nodeName: "node1",
				info: &NodeInfo{
					DefaultDevice: "dev1",
					DisksStatus: map[string]*DiskInfo{
						"dev1": {
							NormalizerName: "normalizer1",
							DiskName:       "dev1Name",
							Capacity: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
							Allocatable: v1alpha1.IOBandwidth{
								Read:  resource.MustParse("500"),
								Write: resource.MustParse("500"),
								Total: resource.MustParse("1000"),
							},
						},
					},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID: "456",
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		diskioresource.IoiContext = &diskioresource.ResourceIOContext{
			PodRequests: map[string]v1alpha1.IOBandwidth{
				"123": {
					Read:  resource.MustParse("200"),
					Write: resource.MustParse("200"),
					Total: resource.MustParse("400"),
				},
			},
		}
		t.Run(tt.name, func(t *testing.T) {
			ps := &Resource{
				nodeName: tt.fields.nodeName,
				info:     tt.fields.info,
				ch:       tt.fields.ch,
			}
			if err := ps.RemovePod(tt.args.pod); (err != nil) != tt.wantErr {
				t.Errorf("Resource.RemovePod() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
