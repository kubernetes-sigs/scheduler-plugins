package diskioaware

import (
	"testing"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

func Test_getScorer(t *testing.T) {
	type args struct {
		scoreStrategy string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "unknown score strategy",
			args: args{
				scoreStrategy: "test",
			},
			wantErr: true,
		},
		{
			name: "MostAllocated",
			args: args{
				scoreStrategy: string(config.MostAllocated),
			},
			wantErr: false,
		},
		{
			name: "LeastAllocated",
			args: args{
				scoreStrategy: string(config.LeastAllocated),
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := getScorer(tt.args.scoreStrategy)
			if (err != nil) != tt.wantErr {
				t.Errorf("getScorer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestMostAllocatedScorer_Score(t *testing.T) {
	type args struct {
		state *stateData
		rh    resource.Handle
	}
	rh := &resource.FakeHandle{
		NodePressureRatioFunc: func(string, v1alpha1.IOBandwidth) (float64, error) {
			return 0.1, nil
		},
	}
	c := fake.NewSimpleClientset()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    args
		want    int64
		wantErr bool
	}{
		{
			name: "calculate score",
			args: args{
				state: &stateData{
					nodeSupportIOI: true,
				},
				rh: rh,
			},
			want:    10,
			wantErr: false,
		},
		{
			name: "calculate score max",
			args: args{
				state: &stateData{
					nodeSupportIOI: false,
				},
				rh: rh,
			},
			want:    100,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scorer := &MostAllocatedScorer{}
			got, err := scorer.Score("", tt.args.state, tt.args.rh)
			if (err != nil) != tt.wantErr {
				t.Errorf("MostAllocatedScorer.Score() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("MostAllocatedScorer.Score() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLeastAllocatedScorer_Score(t *testing.T) {
	type args struct {
		state *stateData
		rh    resource.Handle
	}
	rh := &resource.FakeHandle{
		NodePressureRatioFunc: func(string, v1alpha1.IOBandwidth) (float64, error) {
			return 0.1, nil
		},
	}
	c := fake.NewSimpleClientset()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    args
		want    int64
		wantErr bool
	}{
		{
			name: "calculate score",
			args: args{
				state: &stateData{
					nodeSupportIOI: true,
				},
				rh: rh,
			},
			want:    90,
			wantErr: false,
		},
		{
			name: "calculate score max",
			args: args{
				state: &stateData{
					nodeSupportIOI: false,
				},
				rh: rh,
			},
			want:    100,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scorer := &LeastAllocatedScorer{}
			got, err := scorer.Score("", tt.args.state, tt.args.rh)
			if (err != nil) != tt.wantErr {
				t.Errorf("LeastAllocatedScorer.Score() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("LeastAllocatedScorer.Score() = %v, want %v", got, tt.want)
			}
		})
	}
}
