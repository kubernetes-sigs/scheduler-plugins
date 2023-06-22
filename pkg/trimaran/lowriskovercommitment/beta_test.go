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
package lowriskovercommitment

import (
	"math"
	"reflect"
	"testing"
)

const tolerance float64 = 1e-6

func TestNewBetaDistribution(t *testing.T) {
	type args struct {
		alpha float64
		beta  float64
	}
	/*
		Moments may be computed recursively as follows, where E[X^0]=1,
		E[X^{k}] = \frac { \alpha + k - 1 } { \alpha + \beta + k - 1 } E[X^{k-1}].
	*/
	tests := []struct {
		name string
		args args
		want *BetaDistribution
	}{
		{
			name: "beta(1,1)",
			args: args{
				alpha: 1,
				beta:  1,
			},
			want: &BetaDistribution{
				alpha:        1,
				beta:         1,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 1.0 / 3.0,
				thirdMoment:  0.25,
			},
		},
		{
			name: "beta(1,2)",
			args: args{
				alpha: 1,
				beta:  2,
			},
			want: &BetaDistribution{
				alpha:        1,
				beta:         2,
				isValid:      true,
				firstMoment:  1.0 / 3.0,
				secondMoment: 1.0 / 6.0,
				thirdMoment:  0.1,
			},
		},
		{
			name: "beta(3,1)",
			args: args{
				alpha: 3,
				beta:  1,
			},
			want: &BetaDistribution{
				alpha:        3,
				beta:         1,
				isValid:      true,
				firstMoment:  0.75,
				secondMoment: 0.6,
				thirdMoment:  0.5,
			},
		},
		{
			name: "beta(-1,1)",
			args: args{
				alpha: -1,
				beta:  1,
			},
			want: nil,
		},
		{
			name: "beta(1,-1)",
			args: args{
				alpha: 1,
				beta:  -1,
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewBetaDistribution(tt.args.alpha, tt.args.beta); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewBetaDistribution() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBetaDistribution_MatchMoments(t *testing.T) {
	type fields struct {
		alpha        float64
		beta         float64
		isValid      bool
		firstMoment  float64
		secondMoment float64
		thirdMoment  float64
	}
	type args struct {
		m1 float64
		m2 float64
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		{
			name: "beta(1,1)",
			args: args{
				m1: 0.5,
				m2: 1.0 / 3.0,
			},
			fields: fields{
				alpha:        1,
				beta:         1,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 1.0 / 3.0,
				thirdMoment:  0.25,
			},
			want: true,
		},
		{
			name: "beta(0,0)",
			args: args{
				m1: 0,
				m2: 0,
			},
			fields: fields{},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &BetaDistribution{
				alpha:        tt.fields.alpha,
				beta:         tt.fields.beta,
				isValid:      tt.fields.isValid,
				firstMoment:  tt.fields.firstMoment,
				secondMoment: tt.fields.secondMoment,
				thirdMoment:  tt.fields.thirdMoment,
			}
			if got := b.MatchMoments(tt.args.m1, tt.args.m2); got != tt.want {
				t.Errorf("BetaDistribution.MatchMoments() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBetaDistribution_DensityFunction(t *testing.T) {
	type fields struct {
		alpha        float64
		beta         float64
		isValid      bool
		firstMoment  float64
		secondMoment float64
		thirdMoment  float64
	}
	type args struct {
		x float64
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   float64
	}{
		{
			name: "beta(2,2) pdf(0.5)",
			fields: fields{
				alpha:        2,
				beta:         2,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 0.3,
				thirdMoment:  0.2,
			},
			args: args{
				x: 0.5,
			},
			want: 1.5,
		},
		{
			name: "beta(-1,1) pdf(0.5)",
			fields: fields{
				alpha:   -1,
				beta:    1,
				isValid: false,
			},
			args: args{
				x: 0.5,
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &BetaDistribution{
				alpha:        tt.fields.alpha,
				beta:         tt.fields.beta,
				isValid:      tt.fields.isValid,
				firstMoment:  tt.fields.firstMoment,
				secondMoment: tt.fields.secondMoment,
				thirdMoment:  tt.fields.thirdMoment,
			}
			if got := b.DensityFunction(tt.args.x); !WithinTolerance(got, tt.want, tolerance) {
				t.Errorf("BetaDistribution.DensityFunction() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBetaDistribution_DistributionFunction(t *testing.T) {
	type fields struct {
		alpha        float64
		beta         float64
		isValid      bool
		firstMoment  float64
		secondMoment float64
		thirdMoment  float64
	}
	type args struct {
		x float64
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   float64
	}{
		{
			name: "beta(2,2) PDF(0.5)",
			fields: fields{
				alpha:        2,
				beta:         2,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 0.3,
				thirdMoment:  0.2,
			},
			args: args{
				x: 0.5,
			},
			want: 0.5,
		},
		{
			name: "beta(2,2) PDF(0.0)",
			fields: fields{
				alpha:        2,
				beta:         2,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 0.3,
				thirdMoment:  0.2,
			},
			args: args{
				x: 0.0,
			},
			want: 0.0,
		},
		{
			name: "beta(2,2) PDF(1.0)",
			fields: fields{
				alpha:        2,
				beta:         2,
				isValid:      true,
				firstMoment:  0.5,
				secondMoment: 0.3,
				thirdMoment:  0.2,
			},
			args: args{
				x: 1.0,
			},
			want: 1.0,
		},
		{
			name: "beta(-1,1) PDF(0.5)",
			fields: fields{
				alpha:   -1,
				beta:    1,
				isValid: false,
			},
			args: args{
				x: 0.5,
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &BetaDistribution{
				alpha:        tt.fields.alpha,
				beta:         tt.fields.beta,
				isValid:      tt.fields.isValid,
				firstMoment:  tt.fields.firstMoment,
				secondMoment: tt.fields.secondMoment,
				thirdMoment:  tt.fields.thirdMoment,
			}
			if got := b.DistributionFunction(tt.args.x); !WithinTolerance(got, tt.want, tolerance) {
				t.Errorf("BetaDistribution.DistributionFunction() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetMaxVariance(t *testing.T) {
	type args struct {
		m1 float64
	}
	tests := []struct {
		name string
		args args
		want float64
	}{
		{
			name: "m1=0",
			args: args{
				m1: 0,
			},
			want: 0,
		},
		{
			name: "m1=1",
			args: args{
				m1: 1,
			},
			want: 0,
		},
		{
			name: "m1=0.5",
			args: args{
				m1: 0.5,
			},
			want: 0.25,
		},
		{
			name: "m1=-1",
			args: args{
				m1: -1,
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetMaxVariance(tt.args.m1); !WithinTolerance(got, tt.want, tolerance) {
				t.Errorf("GetMaxVariance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBetaDistribution_Print(t *testing.T) {
	type fields struct {
		alpha        float64
		beta         float64
		isValid      bool
		firstMoment  float64
		secondMoment float64
		thirdMoment  float64
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{
			name: "test0",
			fields: fields{
				alpha:        3,
				beta:         1,
				isValid:      true,
				firstMoment:  0.75,
				secondMoment: 0.6,
				thirdMoment:  0.5,
			},
			want: "BetaDistribution: alpha = 3.000000; beta = 1.000000; mean = 0.750000; var = 0.037500; m1 = 0.750000; m2 = 0.600000; m3 = 0.500000; ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &BetaDistribution{
				alpha:        tt.fields.alpha,
				beta:         tt.fields.beta,
				isValid:      tt.fields.isValid,
				firstMoment:  tt.fields.firstMoment,
				secondMoment: tt.fields.secondMoment,
				thirdMoment:  tt.fields.thirdMoment,
			}
			if got := b.Print(); got != tt.want {
				t.Errorf("BetaDistribution.Print() = %v, want %v", got, tt.want)
			}
		})
	}
}

func WithinTolerance(a, b, e float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) < e
}
