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

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"

	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesources"
	"sigs.k8s.io/scheduler-plugins/pkg/qos"
)

func TestSetup(t *testing.T) {
	// temp dir
	tmpDir, err := ioutil.TempDir("", "scheduler-options")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// https server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"metadata": {"name": "test"}}`))
	}))
	defer server.Close()

	configKubeconfig := filepath.Join(tmpDir, "config.kubeconfig")
	if err := ioutil.WriteFile(configKubeconfig, []byte(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: %s
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    username: config
`, server.URL)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// QOSSort plugin config
	qosSortConfigFile := filepath.Join(tmpDir, "qosSort.yaml")
	if err := ioutil.WriteFile(qosSortConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    queueSort:
      enabled:
      - name: QOSSort
      disabled:
      - name: "*"
    preFilter:
      disabled:
      - name: "*"
    filter:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// Coscheduling plugin config
	coschedulingConfigFile := filepath.Join(tmpDir, "coscheduling.yaml")
	if err := ioutil.WriteFile(coschedulingConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    queueSort:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
    preFilter:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
    filter:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
    permit:
      enabled:
      - name: Coscheduling
    reserve:
      enabled:
      - name: Coscheduling
    postBind:
      enabled:
      - name: Coscheduling
  pluginConfig:
  - name: Coscheduling
    args:
      permitWaitingTimeSeconds: 10
      kubeConfigPath: "%s"
`, configKubeconfig, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// Coscheduling plugin config with arguments
	coschedulingConfigWithArgsFile := filepath.Join(tmpDir, "coscheduling-with-args.yaml")
	if err := ioutil.WriteFile(coschedulingConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    queueSort:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
    preFilter:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
    filter:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
    permit:
      enabled:
      - name: Coscheduling
    reserve:
      enabled:
      - name: Coscheduling
    postBind:
      enabled:
      - name: Coscheduling
  pluginConfig:
  - name: Coscheduling
    args:
      permitWaitingTimeSeconds: 10
      kubeConfigPath: "%s"
`, configKubeconfig, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// NodeResourcesAllocatable plugin config
	nodeResourcesAllocatableConfigFile := filepath.Join(tmpDir, "nodeResourcesAllocatable.yaml")
	if err := ioutil.WriteFile(nodeResourcesAllocatableConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    score:
      enabled:
      - name: NodeResourcesAllocatable
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// NodeResourcesAllocatable plugin config with arguments
	nodeResourcesAllocatableConfigWithArgsFile := filepath.Join(tmpDir, "nodeResourcesAllocatable-with-args.yaml")
	if err := ioutil.WriteFile(nodeResourcesAllocatableConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    score:
      enabled:
      - name: NodeResourcesAllocatable
      disabled:
      - name: "*"
  pluginConfig:
  - name: NodeResourcesAllocatable
    args:
      mode: Least
      resources:
      - name: cpu
        weight: 1000000
      - name: memory
        weight: 1
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// multiple profiles config
	multiProfilesConfig := filepath.Join(tmpDir, "multi-profiles.yaml")
	if err := ioutil.WriteFile(multiProfilesConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- schedulerName: "profile-default-plugins"
- schedulerName: "profile-disable-all-filter-and-score-plugins"
  plugins:
    preFilter:
      disabled:
      - name: "*"
    filter:
      disabled:
      - name: "*"
    postFilter:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	defaultPlugins := map[string][]kubeschedulerconfig.Plugin{
		"QueueSortPlugin": {
			{Name: "PrioritySort"},
		},
		"PreFilterPlugin": {
			{Name: "NodeResourcesFit"},
			{Name: "NodePorts"},
			{Name: "PodTopologySpread"},
			{Name: "InterPodAffinity"},
			{Name: "VolumeBinding"},
		},
		"FilterPlugin": {
			{Name: "NodeUnschedulable"},
			{Name: "NodeResourcesFit"},
			{Name: "NodeName"},
			{Name: "NodePorts"},
			{Name: "NodeAffinity"},
			{Name: "VolumeRestrictions"},
			{Name: "TaintToleration"},
			{Name: "EBSLimits"},
			{Name: "GCEPDLimits"},
			{Name: "NodeVolumeLimits"},
			{Name: "AzureDiskLimits"},
			{Name: "VolumeBinding"},
			{Name: "VolumeZone"},
			{Name: "PodTopologySpread"},
			{Name: "InterPodAffinity"},
		},
		"PostFilterPlugin": {
			{Name: "DefaultPreemption"},
		},
		"PreScorePlugin": {
			{Name: "InterPodAffinity"},
			{Name: "PodTopologySpread"},
			{Name: "TaintToleration"},
			{Name: "SelectorSpread"},
		},
		"ScorePlugin": {
			{Name: "NodeResourcesBalancedAllocation", Weight: 1},
			{Name: "ImageLocality", Weight: 1},
			{Name: "InterPodAffinity", Weight: 1},
			{Name: "NodeResourcesLeastAllocated", Weight: 1},
			{Name: "NodeAffinity", Weight: 1},
			{Name: "NodePreferAvoidPods", Weight: 10000},
			{Name: "PodTopologySpread", Weight: 2},
			{Name: "TaintToleration", Weight: 1},
			{Name: "SelectorSpread", Weight: 1},
		},
		"BindPlugin":    {{Name: "DefaultBinder"}},
		"ReservePlugin": {{Name: "VolumeBinding"}},
		"PreBindPlugin": {{Name: "VolumeBinding"}},
	}

	testcases := []struct {
		name            string
		flags           []string
		registryOptions []app.Option
		wantPlugins     map[string]map[string][]kubeschedulerconfig.Plugin
	}{
		{
			name: "default config",
			flags: []string{
				"--kubeconfig", configKubeconfig,
			},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": defaultPlugins,
			},
		},
		{
			name:            "single profile config - QOSSort",
			flags:           []string{"--config", qosSortConfigFile},
			registryOptions: []app.Option{app.WithPlugin(qos.Name, qos.New)},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": {
					"BindPlugin":       {{Name: "DefaultBinder"}},
					"PostFilterPlugin": {{Name: "DefaultPreemption"}},
					"QueueSortPlugin":  {{Name: "QOSSort"}},
					"ReservePlugin":    {{Name: "VolumeBinding"}},
					"PreBindPlugin":    {{Name: "VolumeBinding"}},
				},
			},
		},
		{
			name:            "single profile config - Coscheduling",
			flags:           []string{"--config", coschedulingConfigFile},
			registryOptions: []app.Option{app.WithPlugin(coscheduling.Name, coscheduling.New)},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": {
					"BindPlugin":       {{Name: "DefaultBinder"}},
					"PreFilterPlugin":  {{Name: "Coscheduling"}},
					"PostBindPlugin":   {{Name: "Coscheduling"}},
					"PostFilterPlugin": {{Name: "DefaultPreemption"}},
					"QueueSortPlugin":  {{Name: "Coscheduling"}},
					"ReservePlugin":    {{Name: "VolumeBinding"}, {Name: "Coscheduling"}},
					"PermitPlugin":     {{Name: "Coscheduling"}},
					"PreBindPlugin":    {{Name: "VolumeBinding"}},
				},
			},
		},
		{
			name:            "single profile config - Coscheduling with args",
			flags:           []string{"--config", coschedulingConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(coscheduling.Name, coscheduling.New)},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": {
					"BindPlugin":       {{Name: "DefaultBinder"}},
					"PreFilterPlugin":  {{Name: "Coscheduling"}},
					"PostBindPlugin":   {{Name: "Coscheduling"}},
					"PostFilterPlugin": {{Name: "DefaultPreemption"}},
					"QueueSortPlugin":  {{Name: "Coscheduling"}},
					"ReservePlugin":    {{Name: "VolumeBinding"}, {Name: "Coscheduling"}},
					"PermitPlugin":     {{Name: "Coscheduling"}},
					"PreBindPlugin":    {{Name: "VolumeBinding"}},
				},
			},
		},
		{
			name:            "single profile config - Node Resources Allocatable",
			flags:           []string{"--config", nodeResourcesAllocatableConfigFile},
			registryOptions: []app.Option{app.WithPlugin(noderesources.AllocatableName, noderesources.NewAllocatable)},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": {
					"BindPlugin":       {{Name: "DefaultBinder"}},
					"FilterPlugin":     defaultPlugins["FilterPlugin"],
					"PostFilterPlugin": {{Name: "DefaultPreemption"}},
					"PreBindPlugin":    {{Name: "VolumeBinding"}},
					"PreFilterPlugin":  defaultPlugins["PreFilterPlugin"],
					"PreScorePlugin":   defaultPlugins["PreScorePlugin"],
					"QueueSortPlugin":  defaultPlugins["QueueSortPlugin"],
					"ReservePlugin":    {{Name: "VolumeBinding"}},
					"ScorePlugin":      {{Name: "NodeResourcesAllocatable", Weight: 1}},
				},
			},
		},
		{
			name:            "single profile config - Node Resources Allocatable with args",
			flags:           []string{"--config", nodeResourcesAllocatableConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(noderesources.AllocatableName, noderesources.NewAllocatable)},
			wantPlugins: map[string]map[string][]kubeschedulerconfig.Plugin{
				"default-scheduler": {
					"BindPlugin":       {{Name: "DefaultBinder"}},
					"FilterPlugin":     defaultPlugins["FilterPlugin"],
					"PostFilterPlugin": {{Name: "DefaultPreemption"}},
					"PreBindPlugin":    {{Name: "VolumeBinding"}},
					"PreFilterPlugin":  defaultPlugins["PreFilterPlugin"],
					"PreScorePlugin":   defaultPlugins["PreScorePlugin"],
					"QueueSortPlugin":  defaultPlugins["QueueSortPlugin"],
					"ReservePlugin":    {{Name: "VolumeBinding"}},
					"ScorePlugin":      {{Name: "NodeResourcesAllocatable", Weight: 1}},
				},
			},
		},
		// TODO: add a multi profile test.
		// Ref: test "plugin config with multiple profiles" in
		// https://github.com/kubernetes/kubernetes/blob/master/cmd/kube-scheduler/app/server_test.go
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("test", pflag.PanicOnError)
			opts, err := options.NewOptions()
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range opts.Flags().FlagSets {
				fs.AddFlagSet(f)
			}
			if err := fs.Parse(tc.flags); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cc, sched, err := app.Setup(ctx, opts, tc.registryOptions...)
			if err != nil {
				t.Fatal(err)
			}
			defer cc.SecureServing.Listener.Close()
			defer cc.InsecureServing.Listener.Close()

			gotPlugins := make(map[string]map[string][]kubeschedulerconfig.Plugin)
			for n, p := range sched.Profiles {
				gotPlugins[n] = p.ListPlugins()
			}

			if diff := cmp.Diff(tc.wantPlugins, gotPlugins); diff != "" {
				t.Errorf("unexpected plugins diff (-want, +got): %s", diff)
			}
		})
	}
}
