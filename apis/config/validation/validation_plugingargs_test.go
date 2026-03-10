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
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"

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

func TestValidateCoschedulingArgs(t *testing.T) {
	testCases := []struct {
		args        *config.CoschedulingArgs
		expectedErr error
		description string
	}{
		{
			description: "correct config with valid values",
			args: &config.CoschedulingArgs{
				PermitWaitingTimeSeconds: 30,
				PodGroupBackoffSeconds:   60,
			},
			expectedErr: nil,
		},
		{
			description: "invalid PermitWaitingTimeSeconds (negative value)",
			args: &config.CoschedulingArgs{
				PermitWaitingTimeSeconds: -10,
				PodGroupBackoffSeconds:   60,
			},
			expectedErr: fmt.Errorf("permitWaitingTimeSeconds: Invalid value: %v: must be greater than 0", -10),
		},
		{
			description: "invalid PodGroupBackoffSeconds (negative value)",
			args: &config.CoschedulingArgs{
				PermitWaitingTimeSeconds: 30,
				PodGroupBackoffSeconds:   -20,
			},
			expectedErr: fmt.Errorf("podGroupBackoffSeconds: Invalid value: %v: must be greater than 0", -20),
		},
		{
			description: "both PermitWaitingTimeSeconds and PodGroupBackoffSeconds are negative",
			args: &config.CoschedulingArgs{
				PermitWaitingTimeSeconds: -30,
				PodGroupBackoffSeconds:   -20,
			},
			expectedErr: fmt.Errorf(
				"[permitWaitingTimeSeconds: Invalid value: %v: must be greater than 0, podGroupBackoffSeconds: Invalid value: %v: must be greater than 0]",
				-30, -20,
			),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			err := ValidateCoschedulingArgs(testCase.args, nil)
			if testCase.expectedErr != nil {
				if err == nil {
					t.Fatalf("expected err to equal %v not nil", testCase.expectedErr)
				}
				if diff := gocmp.Diff(err.Error(), testCase.expectedErr.Error()); diff != "" {
					t.Fatalf("expected err to contain %s in error message: %s", testCase.expectedErr.Error(), err.Error())

				}
			}
			if testCase.expectedErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateNodeResourcesAllocatableArgs(t *testing.T) {
	testCases := []struct {
		args        *config.NodeResourcesAllocatableArgs
		expectedErr error
		description string
	}{
		{
			description: "correct config with valid resources and mode",
			args: &config.NodeResourcesAllocatableArgs{
				Resources: []schedconfig.ResourceSpec{
					{Name: "cpu", Weight: 1},
					{Name: "memory", Weight: 2},
				},
				Mode: config.Least,
			},
			expectedErr: nil,
		},
		{
			description: "invalid resource weight (non-positive value)",
			args: &config.NodeResourcesAllocatableArgs{
				Resources: []schedconfig.ResourceSpec{
					{Name: "cpu", Weight: 0},
					{Name: "memory", Weight: -1},
				},
				Mode: config.Least,
			},
			expectedErr: fmt.Errorf("[resources[0].weight: Invalid value: %v: resource weight of cpu should be a positive value, got :%v, resources[1].weight: Invalid value: %v: resource weight of memory should be a positive value, got :%v]", 0, 0, -1, -1),
		},
		{
			description: "invalid ModeType",
			args: &config.NodeResourcesAllocatableArgs{
				Resources: []schedconfig.ResourceSpec{
					{Name: "cpu", Weight: 1},
					{Name: "memory", Weight: 2},
				},
				Mode: "not existent",
			},
			expectedErr: fmt.Errorf("mode: Invalid value: \"%s\": invalid support ModeType", "not existent"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			err := ValidateNodeResourcesAllocatableArgs(testCase.args, nil)
			if testCase.expectedErr != nil {
				if err == nil {
					t.Fatalf("expected err to equal %v not nil", testCase.expectedErr)
				}
				if diff := gocmp.Diff(err.Error(), testCase.expectedErr.Error()); diff != "" {
					fmt.Println(diff)
					t.Fatalf("expected err to contain %s in error message: %s", testCase.expectedErr.Error(), err.Error())
				}
			}
			if testCase.expectedErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateNodeMetadataArgs(t *testing.T) {
	testCases := []struct {
		args        *config.NodeMetadataArgs
		expectedErr error
		description string
	}{
		{
			description: "correct config with label source and numeric type",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectedErr: nil,
		},
		{
			description: "correct config with annotation source and timestamp type",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "lastUpdate",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
			},
			expectedErr: nil,
		},
		{
			description: "missing MetadataKey",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectedErr: fmt.Errorf("metadataKey cannot be empty"),
		},
		{
			description: "invalid MetadataSource",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  "InvalidSource",
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectedErr: fmt.Errorf("metadataSource must be either \"Label\" or \"Annotation\""),
		},
		{
			description: "invalid MetadataType",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    "InvalidType",
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectedErr: fmt.Errorf("metadataType must be either \"Number\" or \"Timestamp\""),
		},
		{
			description: "invalid ScoringStrategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: "InvalidStrategy",
			},
			expectedErr: fmt.Errorf("scoringStrategy must be one of \"Highest\", \"Lowest\", \"Newest\", or \"Oldest\""),
		},
		{
			description: "numeric type with Newest strategy (mismatch)",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyNewest,
			},
			expectedErr: fmt.Errorf("scoringStrategy \"Newest\" and \"Oldest\" are only valid for metadataType \"Timestamp\""),
		},
		{
			description: "timestamp type with Highest strategy (mismatch)",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "lastUpdate",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectedErr: fmt.Errorf("scoringStrategy \"Highest\" and \"Lowest\" are only valid for metadataType \"Number\""),
		},
		{
			description: "all valid combinations - Lowest with Number",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "cost",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyLowest,
			},
			expectedErr: nil,
		},
		{
			description: "all valid combinations - Oldest with Timestamp",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "provisionedAt",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyOldest,
			},
			expectedErr: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			err := ValidateNodeMetadataArgs(testCase.args, nil)
			if testCase.expectedErr != nil {
				if err == nil {
					t.Fatalf("expected err to equal %v not nil", testCase.expectedErr)
				}
				if !strings.Contains(err.Error(), testCase.expectedErr.Error()) {
					t.Fatalf("expected err to contain %s in error message: %s", testCase.expectedErr.Error(), err.Error())
				}
			}
			if testCase.expectedErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
