/*
Copyright 2020 The Kubernetes Authors.

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

package nodemetadata

import (
	"context"
	"sort"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

func TestCalculateScore(t *testing.T) {
	tests := []struct {
		name        string
		args        *config.NodeMetadataArgs
		node        *v1.Node
		expectError bool
		checkScore  func(int64) bool
	}{
		{
			name: "numeric label - highest strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node1",
					Labels: map[string]string{"priority": "100"},
				},
			},
			expectError: false,
			checkScore:  func(score int64) bool { return score == 100 },
		},
		{
			name: "numeric label - lowest strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "cost",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyLowest,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node1",
					Labels: map[string]string{"cost": "50"},
				},
			},
			expectError: false,
			checkScore:  func(score int64) bool { return score == -50 }, // Inverted for lowest
		},
		{
			name: "timestamp annotation - newest strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "last-update",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
				TimestampFormat: time.RFC3339,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"last-update": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
					},
				},
			},
			expectError: false,
			checkScore:  func(score int64) bool { return score < 0 }, // Negative age
		},
		{
			name: "missing metadata key",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "nonexistent",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node1",
					Labels: map[string]string{"other": "10"},
				},
			},
			expectError: true,
		},
		{
			name: "invalid numeric value",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node1",
					Labels: map[string]string{"priority": "not-a-number"},
				},
			},
			expectError: true,
		},
		{
			name: "invalid timestamp format",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "timestamp",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
				TimestampFormat: time.RFC3339,
			},
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						"timestamp": "not-a-timestamp",
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nm := &NodeMetadata{
				args: tt.args,
			}

			score, err := nm.calculateScore(tt.node)
			if (err != nil) != tt.expectError {
				t.Errorf("calculateScore() error = %v, expectError %v", err, tt.expectError)
				return
			}

			if !tt.expectError && tt.checkScore != nil {
				if !tt.checkScore(score) {
					t.Errorf("calculateScore() score = %v, check failed", score)
				}
			}
		})
	}
}

func TestValidateArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        *config.NodeMetadataArgs
		expectError bool
	}{
		{
			name: "valid numeric args",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectError: false,
		},
		{
			name: "valid timestamp args",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "timestamp",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
				TimestampFormat: time.RFC3339,
			},
			expectError: false,
		},
		{
			name: "missing metadata key",
			args: &config.NodeMetadataArgs{
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectError: true,
		},
		{
			name: "invalid metadata source",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "key",
				MetadataSource:  "Invalid",
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectError: true,
		},
		{
			name: "invalid metadata type",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "key",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    "Invalid",
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			expectError: true,
		},
		{
			name: "timestamp without format",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "timestamp",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
			},
			expectError: true,
		},
		{
			name: "numeric with timestamp strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyNewest,
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArgs(tt.args)
			if (err != nil) != tt.expectError {
				t.Errorf("validateArgs() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestNormalizeScore(t *testing.T) {
	tests := []struct {
		name          string
		args          *config.NodeMetadataArgs
		nodes         []*v1.Node
		expectedOrder []string // nodes ordered from highest to lowest score after normalization
		expectAllZero bool     // if true, all scores should be 0 (MinNodeScore)
	}{
		{
			name: "numeric labels - highest strategy with three nodes",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-low",
						Labels: map[string]string{"priority": "10"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-high",
						Labels: map[string]string{"priority": "100"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-medium",
						Labels: map[string]string{"priority": "50"},
					},
				},
			},
			expectedOrder: []string{"node-high", "node-medium", "node-low"},
		},
		{
			name: "numeric labels - lowest strategy with three nodes",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "cost",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyLowest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-expensive",
						Labels: map[string]string{"cost": "100"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-cheap",
						Labels: map[string]string{"cost": "10"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-medium",
						Labels: map[string]string{"cost": "50"},
					},
				},
			},
			expectedOrder: []string{"node-cheap", "node-medium", "node-expensive"},
		},
		{
			name: "timestamp annotations - newest strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "last-update",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyNewest,
				TimestampFormat: time.RFC3339,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-old",
						Annotations: map[string]string{
							"last-update": time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-newest",
						Annotations: map[string]string{
							"last-update": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-medium",
						Annotations: map[string]string{
							"last-update": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
						},
					},
				},
			},
			expectedOrder: []string{"node-newest", "node-medium", "node-old"},
		},
		{
			name: "timestamp annotations - oldest strategy",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "created-at",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeTimestamp,
				ScoringStrategy: config.ScoringStrategyOldest,
				TimestampFormat: time.RFC3339,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-newest",
						Annotations: map[string]string{
							"created-at": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-oldest",
						Annotations: map[string]string{
							"created-at": time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-medium",
						Annotations: map[string]string{
							"created-at": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
						},
					},
				},
			},
			expectedOrder: []string{"node-oldest", "node-medium", "node-newest"},
		},
		{
			name: "with missing metadata on some nodes",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-with-priority",
						Labels: map[string]string{"priority": "100"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-without-priority",
						Labels: map[string]string{"other": "value"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node-low-priority",
						Labels: map[string]string{"priority": "50"},
					},
				},
			},
			// Node without metadata gets score 0, which after normalization becomes MinNodeScore
			expectedOrder: []string{"node-with-priority", "node-low-priority", "node-without-priority"},
		},
		{
			name: "all nodes missing metadata",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node1",
						Labels: map[string]string{"other": "value"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node2",
						Labels: map[string]string{"other": "value"},
					},
				},
			},
			expectAllZero: true,
		},
		{
			name: "all nodes with same value",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "priority",
				MetadataSource:  config.MetadataSourceLabel,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyHighest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node1",
						Labels: map[string]string{"priority": "50"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node2",
						Labels: map[string]string{"priority": "50"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node3",
						Labels: map[string]string{"priority": "50"},
					},
				},
			},
			expectAllZero: true,
		},
		{
			name: "mixed - some with metadata, some without",
			args: &config.NodeMetadataArgs{
				MetadataKey:     "weight",
				MetadataSource:  config.MetadataSourceAnnotation,
				MetadataType:    config.MetadataTypeNumber,
				ScoringStrategy: config.ScoringStrategyLowest,
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-valid-high",
						Annotations: map[string]string{
							"weight": "200",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-valid-low",
						Annotations: map[string]string{
							"weight": "100",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-valid-medium",
						Annotations: map[string]string{
							"weight": "150",
						},
					},
				},
			},
			// For lowest strategy: lower is better (100 < 150 < 200)
			expectedOrder: []string{"node-valid-low", "node-valid-medium", "node-valid-high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			nm := &NodeMetadata{
				args: tt.args,
			}

			// Create framework.NodeScoreList with raw scores
			nodeScores := framework.NodeScoreList{}
			for _, node := range tt.nodes {
				score, err := nm.calculateScore(node)
				if err != nil {
					// For nodes with missing or invalid metadata, score should be 0
					score = 0
				}
				nodeScores = append(nodeScores, framework.NodeScore{
					Name:  node.Name,
					Score: score,
				})
			}

			// Call the actual NormalizeScore method
			status := nm.NormalizeScore(ctx, nil, &v1.Pod{}, nodeScores)
			if !status.IsSuccess() {
				t.Fatalf("NormalizeScore failed: %v", status.AsError())
			}

			if tt.expectAllZero {
				// All scores should be MinNodeScore (0)
				for _, ns := range nodeScores {
					if ns.Score != framework.MinNodeScore {
						t.Errorf("Expected all scores to be %d (MinNodeScore), but %s has score %d", framework.MinNodeScore, ns.Name, ns.Score)
					}
				}
				return
			}

			// Verify the order matches expected
			if len(tt.expectedOrder) > 0 {
				// Sort nodeScores by score (descending)
				sortedScores := make([]framework.NodeScore, len(nodeScores))
				copy(sortedScores, nodeScores)

				sort.Slice(sortedScores, func(i, j int) bool {
					return sortedScores[i].Score > sortedScores[j].Score
				})

				// Check if order matches
				for i, expectedName := range tt.expectedOrder {
					if i >= len(sortedScores) {
						t.Errorf("Expected node %s at position %d, but not enough nodes in result", expectedName, i)
						continue
					}
					if sortedScores[i].Name != expectedName {
						t.Errorf("Position %d: expected node %s (score: %d), got %s (score: %d)",
							i, expectedName, getScoreByName(sortedScores, expectedName),
							sortedScores[i].Name, sortedScores[i].Score)
					}
				}

				// Verify scores are properly normalized (highest should be MaxNodeScore, lowest should be MinNodeScore)
				if len(sortedScores) > 0 {
					if sortedScores[0].Score != framework.MaxNodeScore {
						t.Errorf("Highest score should be %d, got %d for node %s", framework.MaxNodeScore, sortedScores[0].Score, sortedScores[0].Name)
					}
					if sortedScores[len(sortedScores)-1].Score != framework.MinNodeScore {
						t.Errorf("Lowest score should be %d, got %d for node %s", framework.MinNodeScore, sortedScores[len(sortedScores)-1].Score, sortedScores[len(sortedScores)-1].Name)
					}
				}
			}
		})
	}
}

// Helper function to get score by node name
func getScoreByName(scores []framework.NodeScore, name string) int64 {
	for _, s := range scores {
		if s.Name == name {
			return s.Score
		}
	}
	return -1
}
