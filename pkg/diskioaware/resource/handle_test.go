package resource

import (
	"reflect"
	"testing"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	v1 "k8s.io/api/core/v1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestHandleBase_RemovePod(t *testing.T) {
	testPod := st.MakePod().Name("pod1").Namespace("default").Obj()

	type fields struct {
		Pod2Add *v1.Pod
	}
	type args struct {
		pod *v1.Pod
	}
	tests := []struct {
		name    string
		args    args
		fields  fields
		wantErr bool
	}{
		{
			name: "Remove pod",
			fields: fields{
				Pod2Add: testPod,
			},
			args: args{
				pod: testPod,
			},
			wantErr: false,
		},
		{
			name: "Remove pod which does not exist",
			fields: fields{
				Pod2Add: nil,
			},
			args: args{
				pod: testPod,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAddPod *v1.Pod
			var gotRemovePod *v1.Pod
			tn1 := "testNodeA"
			ec1 := NewExtendedCache()
			ec1.SetExtendedResource(tn1, &FakeResource{
				AddPodFunc: func(pod *v1.Pod, bw v1alpha1.IOBandwidth) error {
					gotAddPod = pod
					return nil
				},
				RemovePodFunc: func(pod *v1.Pod) error {
					gotRemovePod = pod
					return nil
				},
			})
			h := &HandleBase{
				EC: ec1,
			}
			if tt.fields.Pod2Add != nil {
				err := h.AddPod(tt.fields.Pod2Add, tn1, v1alpha1.IOBandwidth{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := h.RemovePod(tt.args.pod, tn1); (err != nil) != tt.wantErr {
				t.Errorf("HandleBase.RemovePod() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.fields.Pod2Add != nil && !reflect.DeepEqual(gotAddPod, gotRemovePod) {
				t.Errorf("HandleBase.RemovePod() gotAddPod = %v, gotRemovePod = %v", gotAddPod, gotRemovePod)
			}
		})
	}
}

func TestHandleBase_PrintCacheInfo(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "Print cache info",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HandleBase{
				EC: NewExtendedCache(),
			}
			h.PrintCacheInfo()
		})
	}
}

func TestHandleBase_AddPod(t *testing.T) {
	testPod := st.MakePod().Name("pod1").Namespace("default").Obj()
	tn1 := "testNodeA"

	type args struct {
		pod      *v1.Pod
		nodeName string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "Add pod",
			args: args{
				pod:      testPod,
				nodeName: tn1,
			},
			wantErr: false,
		},
		{
			name: "Add nil pod",
			args: args{
				pod:      nil,
				nodeName: tn1,
			},
			wantErr: false,
		},
		{
			name: "Add pod to unknown node",
			args: args{
				pod:      nil,
				nodeName: "unkown",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAddPod *v1.Pod
			ec1 := NewExtendedCache()
			ec1.SetExtendedResource(tn1, &FakeResource{
				AddPodFunc: func(pod *v1.Pod, _ v1alpha1.IOBandwidth) error {
					gotAddPod = pod
					return nil
				},
			})
			h := &HandleBase{
				EC: ec1,
			}
			err := h.AddPod(tt.args.pod, tt.args.nodeName, v1alpha1.IOBandwidth{})
			if (err != nil) != tt.wantErr {
				t.Errorf("HandleBase.AddPod() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(gotAddPod, tt.args.pod) {
				t.Errorf("HandleBase.AddPod() got = %v, want %v", gotAddPod, tt.args.pod)
			}
		})
	}
}

func TestHandleBase_NodeRegistered(t *testing.T) {
	tn1 := "testNodeA"
	ec1 := NewExtendedCache()
	ec1.SetExtendedResource(tn1, &FakeResource{})
	tn2 := "testNodeB"
	ec2 := NewExtendedCache()
	ec2.SetExtendedResource(tn2, &FakeResource{})
	type fields struct {
		EC ExtendedCache
	}
	type args struct {
		node string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		{
			name: "Node registered",
			fields: fields{
				EC: ec1,
			},
			args: args{
				node: tn1,
			},
			want: true,
		},
		{
			name: "Node is not registered",
			fields: fields{
				EC: ec1,
			},
			args: args{
				node: tn2,
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			h := &HandleBase{
				EC: tt.fields.EC,
			}
			if got := h.NodeRegistered(tt.args.node); got != tt.want {
				t.Errorf("HandleBase.NodeRegistered() = %v, want %v", got, tt.want)
			}
		})
	}
}
