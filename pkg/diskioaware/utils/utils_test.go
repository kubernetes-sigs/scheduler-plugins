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

package utils

import (
	"context"
	"reflect"
	"testing"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	fakeioiclientset "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRequestStrToQuantity(t *testing.T) {
	type args struct {
		reqStr string
	}
	tests := []struct {
		name    string
		args    args
		want    v1alpha1.IOBandwidth
		wantErr bool
	}{
		{
			name: "Test RequestStrToQuantity success",
			args: args{
				reqStr: `{"read":"1000","write":"500"}`,
			},
			want: v1alpha1.IOBandwidth{
				Read:  resource.MustParse("1000"),
				Write: resource.MustParse("500"),
				Total: resource.MustParse("1500"),
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RequestStrToQuantity(tt.args.reqStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("RequestStrToQuantity() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got.Read.String(), tt.want.Read.String()) || !reflect.DeepEqual(got.Write.String(), tt.want.Write.String()) || !reflect.DeepEqual(got.Total.String(), tt.want.Total.String()) {
				t.Errorf("RequestStrToQuantity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComparePodList(t *testing.T) {
	type args struct {
		pl1 []string
		pl2 []string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "Test ComparePodList success",
			args: args{
				pl1: []string{"pod1", "pod2", "pod3"},
				pl2: []string{"pod3", "pod2", "pod1"},
			},
			want: true,
		},
		{
			name: "Test ComparePodList fail#1",
			args: args{
				pl1: []string{"pod1", "pod2", "pod3"},
				pl2: []string{"pod4", "pod2", "pod1"},
			},
			want: false,
		},
		{
			name: "Test ComparePodList fail#2",
			args: args{
				pl1: []string{"pod1", "pod2", "pod3"},
				pl2: []string{"pod2", "pod1"},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComparePodList(tt.args.pl1, tt.args.pl2); got != tt.want {
				t.Errorf("ComparePodList() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHashObject(t *testing.T) {
	type args struct {
		obj interface{}
	}
	ref := v1alpha1.IOBandwidth{
		Read:  resource.MustParse("1000"),
		Write: resource.MustParse("500"),
		Total: resource.MustParse("1500"),
	}
	refHash := HashObject(ref)
	tests := []struct {
		name string
		args args
		want uint64
	}{
		{
			name: "Test HashObject success",
			args: args{
				obj: v1alpha1.IOBandwidth{
					Read:  resource.MustParse("1000"),
					Write: resource.MustParse("500"),
					Total: resource.MustParse("1500"),
				},
			},
			want: refHash,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HashObject(tt.args.obj); got != tt.want {
				t.Errorf("HashObject() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCreateNodeIOStatus(t *testing.T) {
	type args struct {
		ctx    context.Context
		client versioned.Interface
		node   string
		pl     []string
	}
	fakeClient := fakeioiclientset.NewSimpleClientset()
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "Test CreateNodeIOStatus success",
			args: args{
				ctx:    context.TODO(),
				client: fakeClient,
				node:   "node1",
				pl:     []string{"pod1", "pod2", "pod3"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CreateNodeIOStatus(tt.args.ctx, tt.args.client, tt.args.node, tt.args.pl); (err != nil) != tt.wantErr {
				t.Errorf("CreateNodeIOStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetNodeIOStatus(t *testing.T) {
	type args struct {
		ctx    context.Context
		client versioned.Interface
		n      string
	}
	fakeClient := fakeioiclientset.NewSimpleClientset()
	err := CreateNodeIOStatus(context.TODO(), fakeClient, "node1", []string{"pod1", "pod2", "pod3"})
	if err != nil {
		t.Error(err)
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "Test GetNodeIOStatus success",
			args: args{
				ctx:    context.TODO(),
				client: fakeClient,
				n:      "node1",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GetNodeIOStatus(tt.args.ctx, tt.args.client, tt.args.n)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetNodeIOStatus() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestUpdateNodeIOStatus(t *testing.T) {
	type args struct {
		ctx    context.Context
		client versioned.Interface
		node   string
		pl     []string
	}
	fakeClient := fakeioiclientset.NewSimpleClientset()
	err := CreateNodeIOStatus(context.TODO(), fakeClient, "node1", []string{"pod1", "pod2", "pod3"})
	if err != nil {
		t.Error(err)
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "Test UpdateNodeIOStatus success",
			args: args{
				ctx:    context.TODO(),
				client: fakeClient,
				node:   "node1",
				pl:     []string{"pod1", "pod2", "pod3", "pod4"},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := UpdateNodeIOStatus(tt.args.ctx, tt.args.client, tt.args.node, tt.args.pl); (err != nil) != tt.wantErr {
				t.Errorf("UpdateNodeIOStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
