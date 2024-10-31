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

package validation

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

func TestValidateNodeResourceTopologyMatchArgs(t *testing.T) {
	testCases := []struct {
		args        *config.NodeResourceTopologyMatchArgs
		expectedErr error
		description string
	}{
		{
			description: "correct config",
			args: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: config.MostAllocated,
				},
			},
		},
		{
			description: "incorrect config, wrong ScoringStrategy type",
			args: &config.NodeResourceTopologyMatchArgs{
				ScoringStrategy: config.ScoringStrategy{
					Type: "not existent",
				},
			},
			expectedErr: fmt.Errorf("scoringStrategy.type: Invalid value:"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			err := ValidateNodeResourceTopologyMatchArgs(nil, testCase.args)
			if testCase.expectedErr != nil {
				if err == nil {
					t.Errorf("expected err to equal %v not nil", testCase.expectedErr)
				}

				if !strings.Contains(err.Error(), testCase.expectedErr.Error()) {
					t.Errorf("expected err to contain %s in error message: %s", testCase.expectedErr.Error(), err.Error())
				}
			}
			if testCase.expectedErr == nil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateDiskIOArgs(t *testing.T) {
	tempFile, err := os.CreateTemp(os.TempDir(), "validateDiskIOArgs")
	if err != nil {
		t.Fatal("Failed to create temp file", err)
	}
	defer os.Remove(tempFile.Name())
	testCases := []struct {
		args        *config.DiskIOArgs
		expectedErr error
		description string
	}{
		{
			description: "correct config",
			args: &config.DiskIOArgs{
				ScoreStrategy:     string(config.LeastAllocated),
				NSWhiteList:       []string{"ns1", "ns2"},
				DiskIOModelConfig: tempFile.Name(),
			},
		},
		{
			description: "incorrect config, wrong ScoreStrategy type",
			args: &config.DiskIOArgs{
				ScoreStrategy: "UnknownStrategy",
				NSWhiteList:   nil,
			},
			expectedErr: fmt.Errorf("scoreStrategy: Invalid value"),
		},
		{
			description: "incorrect config, wrong namespace format",
			args: &config.DiskIOArgs{
				ScoreStrategy: string(config.LeastAllocated),
				NSWhiteList:   []string{"!!!!"},
			},
			expectedErr: fmt.Errorf("invalid namespace format"),
		},
		{
			description: "incorrect config, wrong model config path",
			args: &config.DiskIOArgs{
				ScoreStrategy:     string(config.LeastAllocated),
				NSWhiteList:       []string{"kube-system"},
				DiskIOModelConfig: "!@#$$",
			},
			expectedErr: fmt.Errorf("invalid DiskIOModelConfig"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			err := ValidateDiskIOArgs(nil, testCase.args)
			if testCase.expectedErr != nil {
				if err == nil {
					t.Errorf("expected err to equal %v not nil", testCase.expectedErr)
				}

				if !strings.Contains(err.Error(), testCase.expectedErr.Error()) {
					t.Errorf("expected err to contain %s in error message: %s", testCase.expectedErr.Error(), err.Error())
				}
			}
			if testCase.expectedErr == nil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
