/*
Copyright 2021 The Kubernetes Authors.

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

package flags

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"k8s.io/klog/v2"
)

const (
	flagNamespaceName      = "namespace"
	flagKubecofigName      = "kubeconfig"
	flagFeatureName        = "feature"
	flagAssessName         = "assess"
	flagLabelsName         = "labels"
	flagSkipLabelName      = "skip-labels"
	flagSkipFeatureName    = "skip-features"
	flagSkipAssessmentName = "skip-assessment"
	flagParallelTestsName  = "parallel"
)

// Supported flag definitions
var (
	featureFlag = flag.Flag{
		Name:  flagFeatureName,
		Usage: "Regular expression to select feature(s) to test",
	}
	assessFlag = flag.Flag{
		Name:  flagAssessName,
		Usage: "Regular expression to select assessment(s) to run",
	}
	labelsFlag = flag.Flag{
		Name:  flagLabelsName,
		Usage: "Comma-separated key=value to filter features by labels",
	}
	kubecfgFlag = flag.Flag{
		Name:  flagKubecofigName,
		Usage: "Path to a cluster kubeconfig file (optional)",
	}
	kubeNSFlag = flag.Flag{
		Name:  flagNamespaceName,
		Usage: "A namespace value to use for testing (optional)",
	}
	skipLabelsFlag = flag.Flag{
		Name:  flagSkipLabelName,
		Usage: "Regular expression to skip label(s) to run",
	}
	skipFeatureFlag = flag.Flag{
		Name:  flagSkipFeatureName,
		Usage: "Regular expression to skip feature(s) to run",
	}
	skipAssessmentFlag = flag.Flag{
		Name:  flagSkipAssessmentName,
		Usage: "Regular expression to skip assessment(s) to run",
	}
	parallelTestsFlag = flag.Flag{
		Name:  flagParallelTestsName,
		Usage: "Run test features in parallel",
	}
)

// EnvFlags surfaces all resolved flag values for the testing framework
type EnvFlags struct {
	feature         string
	assess          string
	labels          LabelsMap
	kubeconfig      string
	namespace       string
	skiplabels      LabelsMap
	skipFeatures    string
	skipAssessments string
	parallelTests   bool
}

// Feature returns value for `-feature` flag
func (f *EnvFlags) Feature() string {
	return f.feature
}

// Assessment returns value for `-assess` flag
func (f *EnvFlags) Assessment() string {
	return f.assess
}

// Labels returns a map of parsed key/value from `-labels` flag
func (f *EnvFlags) Labels() LabelsMap {
	return f.labels
}

// Namespace returns an optional namespace flag value
func (f *EnvFlags) Namespace() string {
	return f.namespace
}

func (f *EnvFlags) SkipFeatures() string {
	return f.skipFeatures
}

func (f *EnvFlags) SkipAssessment() string {
	return f.skipAssessments
}

func (f *EnvFlags) SkipLabels() LabelsMap {
	return f.skiplabels
}

// Kubeconfig returns an optional path for kubeconfig file
func (f *EnvFlags) Kubeconfig() string {
	return f.kubeconfig
}

func (f *EnvFlags) Parallel() bool {
	return f.parallelTests
}

// Parse parses defined CLI args os.Args[1:]
func Parse() (*EnvFlags, error) {
	return ParseArgs(os.Args[1:])
}

// ParseArgs parses the specified args from global flag.CommandLine
// and returns a set of environment flag values.
func ParseArgs(args []string) (*EnvFlags, error) {
	var (
		feature        string
		assess         string
		namespace      string
		kubeconfig     string
		skipFeature    string
		skipAssessment string
		parallelTests  bool
	)

	labels := make(LabelsMap)
	skipLabels := make(LabelsMap)

	if flag.Lookup(featureFlag.Name) == nil {
		flag.StringVar(&feature, featureFlag.Name, featureFlag.DefValue, featureFlag.Usage)
	}

	if flag.Lookup(assessFlag.Name) == nil {
		flag.StringVar(&assess, assessFlag.Name, assessFlag.DefValue, assessFlag.Usage)
	}

	if flag.Lookup(kubecfgFlag.Name) == nil {
		flag.StringVar(&kubeconfig, kubecfgFlag.Name, kubecfgFlag.DefValue, kubecfgFlag.Usage)
	}

	if flag.Lookup(kubeNSFlag.Name) == nil {
		flag.StringVar(&namespace, kubeNSFlag.Name, kubeNSFlag.DefValue, kubeNSFlag.Usage)
	}

	if flag.Lookup(labelsFlag.Name) == nil {
		flag.Var(&labels, labelsFlag.Name, labelsFlag.Usage)
	}

	if flag.Lookup(skipLabelsFlag.Name) == nil {
		flag.Var(&skipLabels, skipLabelsFlag.Name, skipLabelsFlag.Usage)
	}

	if flag.Lookup(skipAssessmentFlag.Name) == nil {
		flag.StringVar(&skipAssessment, skipAssessmentFlag.Name, skipAssessmentFlag.DefValue, skipAssessmentFlag.Usage)
	}

	if flag.Lookup(skipFeatureFlag.Name) == nil {
		flag.StringVar(&skipFeature, skipFeatureFlag.Name, skipFeatureFlag.DefValue, skipFeatureFlag.Usage)
	}

	if flag.Lookup(parallelTestsFlag.Name) == nil {
		flag.BoolVar(&parallelTests, parallelTestsFlag.Name, false, parallelTestsFlag.Usage)
	}

	// Enable klog/v2 flag integration
	klog.InitFlags(nil)

	if err := flag.CommandLine.Parse(args); err != nil {
		return nil, fmt.Errorf("flags parsing: %w", err)
	}

	return &EnvFlags{
		feature:         feature,
		assess:          assess,
		labels:          labels,
		namespace:       namespace,
		kubeconfig:      kubeconfig,
		skiplabels:      skipLabels,
		skipFeatures:    skipFeature,
		skipAssessments: skipAssessment,
		parallelTests:   parallelTests,
	}, nil
}

type LabelsMap map[string]string

func (m LabelsMap) String() string {
	i := map[string]string(m)
	return fmt.Sprint(i)
}

func (m LabelsMap) Set(val string) error {
	// label: []string{"key=value",...}
	for _, label := range strings.Split(val, ",") {
		// split into k,v
		kv := strings.Split(label, "=")
		if len(kv) != 2 {
			return fmt.Errorf("label format error: %s", label)
		}
		m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}

	return nil
}
