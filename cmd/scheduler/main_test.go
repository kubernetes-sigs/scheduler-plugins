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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"

	"k8s.io/kubernetes/cmd/kube-scheduler/app"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/testing/defaults"

	"sigs.k8s.io/scheduler-plugins/pkg/capacityscheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/coscheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/networkaware/networkoverhead"
	"sigs.k8s.io/scheduler-plugins/pkg/networkaware/topologicalsort"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesources"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology"
	"sigs.k8s.io/scheduler-plugins/pkg/podstate"
	"sigs.k8s.io/scheduler-plugins/pkg/qos"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/loadvariationriskbalancing"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/lowriskovercommitment"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/targetloadpacking"
)

func TestSetup(t *testing.T) {
	// temp dir
	tmpDir, err := os.MkdirTemp("", "scheduler-options")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// https server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"metadata": {"name": "test"}}`))
	}))
	defer server.Close()

	configKubeconfig := filepath.Join(tmpDir, "config.kubeconfig")
	if err := os.WriteFile(configKubeconfig, []byte(fmt.Sprintf(`
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

	// PodState plugin config
	podStateConfigFile := filepath.Join(tmpDir, "podState.yaml")
	if err := os.WriteFile(podStateConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
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
      enabled:
      - name: PodState
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// QOSSort plugin config
	qosSortConfigFile := filepath.Join(tmpDir, "qosSort.yaml")
	if err := os.WriteFile(qosSortConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
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
	if err := os.WriteFile(coschedulingConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    multiPoint:
      enabled:
      - name: Coscheduling
    queueSort:
      disabled:
      - name: PrioritySort
    filter:
      disabled:
      - name: "*"
    score:
      disabled:
      - name: "*"
    preScore:
      disabled:
      - name: "*"
  pluginConfig:
  - name: Coscheduling
    args:
      permitWaitingTimeSeconds: 10
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// NodeResourcesAllocatable plugin config with arguments
	nodeResourcesAllocatableConfigWithArgsFile := filepath.Join(tmpDir, "nodeResourcesAllocatable-with-args.yaml")
	if err := os.WriteFile(nodeResourcesAllocatableConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
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

	// CapacityScheduling plugin config with arguments
	capacitySchedulingConfigv1beta2 := filepath.Join(tmpDir, "capacityScheduling-v1beta2.yaml")
	if err := os.WriteFile(capacitySchedulingConfigv1beta2, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- schedulerName: default-scheduler
  plugins:
    preFilter:
      enabled:
      - name: CapacityScheduling
    postFilter:
      enabled:
      - name: CapacityScheduling
      disabled:
      - name: "*"
    reserve:
      enabled:
      - name: CapacityScheduling
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}
	capacitySchedulingConfigv1beta3 := filepath.Join(tmpDir, "capacityScheduling-v1beta3.yaml")
	if err := os.WriteFile(capacitySchedulingConfigv1beta3, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- schedulerName: default-scheduler
  plugins:
    preFilter:
      enabled:
      - name: CapacityScheduling
    postFilter:
      enabled:
      - name: CapacityScheduling
      disabled:
      - name: "*"
    reserve:
      enabled:
      - name: CapacityScheduling
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// TargetLoadPacking plugin config with arguments
	targetLoadPackingConfigWithArgsFile := filepath.Join(tmpDir, "targetLoadPacking-with-args.yaml")
	if err := os.WriteFile(targetLoadPackingConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    score:
      enabled:
      - name: TargetLoadPacking
      disabled:
      - name: "*"
  pluginConfig:
  - name: TargetLoadPacking
    args:
      targetUtilization: 60 
      defaultRequests:
        cpu: "1000m"
      defaultRequestsMultiplier: "1.8"
      watcherAddress: http://deadbeef:2020
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// TargetLoadPacking plugin config with Prometheus Metric Provider arguments
	targetLoadPackingConfigWithPrometheusArgsFile := filepath.Join(tmpDir, "targetLoadPacking-with-prometheus-args.yaml")
	if err := os.WriteFile(targetLoadPackingConfigWithPrometheusArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    score:
      enabled:
      - name: TargetLoadPacking
      disabled:
      - name: "*"
  pluginConfig:
  - name: TargetLoadPacking
    args:
      metricProvider:
        type: Prometheus
        address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
        insecureSkipVerify: false
      targetUtilization: 60 
      defaultRequests:
        cpu: "1000m"
      defaultRequestsMultiplier: "1.8"
      watcherAddress: http://deadbeef:2020
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// LoadVariationRiskBalancing plugin config with arguments
	loadVariationRiskBalancingConfigWithArgsFile := filepath.Join(tmpDir, "loadVariationRiskBalancing-with-args.yaml")
	if err := os.WriteFile(loadVariationRiskBalancingConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    score:
      enabled:
      - name: LoadVariationRiskBalancing
      disabled:
      - name: "*"
  pluginConfig:
  - name: LoadVariationRiskBalancing
    args:
      metricProvider:
        type: Prometheus
        address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
      safeVarianceMargin: 1
      safeVarianceSensitivity: 2.5
      watcherAddress: http://deadbeef:2020
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// LowRiskOverCommitment plugin config with arguments
	lowRiskOverCommitmentConfigWithArgsFile := filepath.Join(tmpDir, "lowRiskOverCommitment-with-args.yaml")
	if err := os.WriteFile(lowRiskOverCommitmentConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    preScore:
      enabled:
      - name: LowRiskOverCommitment
      disabled:
      - name: "*"
    score:
      enabled:
      - name: LowRiskOverCommitment
      disabled:
      - name: "*"
  pluginConfig:
  - name: LowRiskOverCommitment
    args:
      metricProvider:
        type: Prometheus
        address: http://prometheus-k8s.monitoring.svc.cluster.local:9090
      smoothingWindowSize: 5
      riskLimitWeights:
        cpu: 0.5
        memory: 0.5
      watcherAddress: http://deadbeef:2020
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// NodeResourceTopologyMatch plugin config
	nodeResourceTopologyMatchConfigWithArgsFile := filepath.Join(tmpDir, "nodeResourceTopologyMatch.yaml")
	if err := os.WriteFile(nodeResourceTopologyMatchConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    filter:
      enabled:
      - name: NodeResourceTopologyMatch
      disabled:
      - name: "*"
    score:
      enabled:
      - name: NodeResourceTopologyMatch
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// topologicalSort plugin config
	topologicalSortConfigFile := filepath.Join(tmpDir, "topologicalSort.yaml")
	if err := os.WriteFile(topologicalSortConfigFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    queueSort:
      enabled:
      - name: TopologicalSort
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

	// networkOverhead plugin config
	networkOverheadConfigWithArgsFile := filepath.Join(tmpDir, "networkOverhead.yaml")
	if err := os.WriteFile(networkOverheadConfigWithArgsFile, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: "%s"
profiles:
- plugins:
    preFilter:
      enabled:
      - name: NetworkOverhead
    filter:
      enabled:
      - name: NetworkOverhead
      disabled:
      - name: "*"
    score:
      enabled:
      - name: NetworkOverhead
      disabled:
      - name: "*"
`, configKubeconfig)), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	// multiple profiles config
	multiProfilesConfig := filepath.Join(tmpDir, "multi-profiles.yaml")
	if err := os.WriteFile(multiProfilesConfig, []byte(fmt.Sprintf(`
apiVersion: kubescheduler.config.k8s.io/v1beta3
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

	testcases := []struct {
		name            string
		flags           []string
		registryOptions []app.Option
		wantPlugins     map[string]*config.Plugins
	}{
		{
			name: "default config",
			flags: []string{
				"--kubeconfig", configKubeconfig,
			},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": defaults.ExpandedPluginsV1,
			},
		},
		{
			name:            "single profile config - PodState - v1beta2",
			flags:           []string{"--config", podStateConfigFile},
			registryOptions: []app.Option{app.WithPlugin(podstate.Name, podstate.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.PluginsV1beta2.PreEnqueue,
					QueueSort:  defaults.PluginsV1beta2.QueueSort,
					Bind:       defaults.PluginsV1beta2.Bind,
					PostFilter: defaults.PluginsV1beta2.PostFilter,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: podstate.Name, Weight: 1}}},
					Reserve:    defaults.PluginsV1beta2.Reserve,
					PreBind:    defaults.PluginsV1beta2.PreBind,
				},
			},
		},
		{
			name:            "single profile config - QOSSort",
			flags:           []string{"--config", qosSortConfigFile},
			registryOptions: []app.Option{app.WithPlugin(qos.Name, qos.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: qos.Name}}},
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - Coscheduling",
			flags:           []string{"--config", coschedulingConfigFile},
			registryOptions: []app.Option{app.WithPlugin(coscheduling.Name, coscheduling.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					MultiPoint: defaults.ExpandedPluginsV1.MultiPoint,
					PreEnqueue: defaults.ExpandedPluginsV1.PreEnqueue,
					QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: coscheduling.Name}}},
					Bind:       defaults.ExpandedPluginsV1.Bind,
					PreFilter: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1.PreFilter.Enabled, config.Plugin{Name: coscheduling.Name}),
					},
					PostFilter: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1.PostFilter.Enabled, config.Plugin{Name: coscheduling.Name}),
					},
					Permit: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1.Permit.Enabled, config.Plugin{Name: coscheduling.Name}),
					},
					Reserve: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1.Reserve.Enabled, config.Plugin{Name: coscheduling.Name}),
					},
					PreBind: defaults.ExpandedPluginsV1.PreBind,
				},
			},
		},
		{
			name:            "single profile config - Node Resources Allocatable with args",
			flags:           []string{"--config", nodeResourcesAllocatableConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(noderesources.AllocatableName, noderesources.NewAllocatable)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1.QueueSort,
					Bind:       defaults.ExpandedPluginsV1.Bind,
					PreFilter:  defaults.ExpandedPluginsV1.PreFilter,
					Filter:     defaults.ExpandedPluginsV1.Filter,
					PostFilter: defaults.ExpandedPluginsV1.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: noderesources.AllocatableName, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1.Reserve,
					PreBind:    defaults.ExpandedPluginsV1.PreBind,
				},
			},
		},
		{
			name:            "single profile config - Capacityscheduling - v1beta2",
			flags:           []string{"--config", capacitySchedulingConfigv1beta2},
			registryOptions: []app.Option{app.WithPlugin(capacityscheduling.Name, capacityscheduling.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.PluginsV1beta2.PreEnqueue,
					QueueSort:  defaults.PluginsV1beta2.QueueSort,
					Bind:       defaults.PluginsV1beta2.Bind,
					PreFilter: config.PluginSet{
						Enabled: append(defaults.PluginsV1beta2.PreFilter.Enabled, config.Plugin{Name: capacityscheduling.Name}),
					},
					Filter:     defaults.PluginsV1beta2.Filter,
					PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: capacityscheduling.Name}}},
					PreScore:   defaults.PluginsV1beta2.PreScore,
					Score:      defaults.PluginsV1beta2.Score,
					Reserve: config.PluginSet{
						Enabled: append(defaults.PluginsV1beta2.Reserve.Enabled, config.Plugin{Name: capacityscheduling.Name}),
					},
					PreBind: defaults.PluginsV1beta2.PreBind,
				},
			},
		},
		{
			name:            "single profile config - Capacityscheduling - v1beta3",
			flags:           []string{"--config", capacitySchedulingConfigv1beta3},
			registryOptions: []app.Option{app.WithPlugin(capacityscheduling.Name, capacityscheduling.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1beta3.PreFilter.Enabled, config.Plugin{Name: capacityscheduling.Name}),
					},
					Filter:     defaults.ExpandedPluginsV1beta3.Filter,
					PostFilter: config.PluginSet{Enabled: []config.Plugin{{Name: capacityscheduling.Name}}},
					PreScore:   defaults.ExpandedPluginsV1beta3.PreScore,
					Score:      defaults.ExpandedPluginsV1beta3.Score,
					Reserve: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1beta3.Reserve.Enabled, config.Plugin{Name: capacityscheduling.Name}),
					},
					PreBind: defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - TargetLoadPacking with args",
			flags:           []string{"--config", targetLoadPackingConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(targetloadpacking.Name, targetloadpacking.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter:  defaults.ExpandedPluginsV1beta3.PreFilter,
					Filter:     defaults.ExpandedPluginsV1beta3.Filter,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1beta3.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: targetloadpacking.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - TargetLoadPacking with prometheus metric provider args",
			flags:           []string{"--config", targetLoadPackingConfigWithPrometheusArgsFile},
			registryOptions: []app.Option{app.WithPlugin(targetloadpacking.Name, targetloadpacking.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter:  defaults.ExpandedPluginsV1beta3.PreFilter,
					Filter:     defaults.ExpandedPluginsV1beta3.Filter,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1beta3.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: targetloadpacking.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - LoadVariationRiskBalancing with args",
			flags:           []string{"--config", loadVariationRiskBalancingConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(loadvariationriskbalancing.Name, loadvariationriskbalancing.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter:  defaults.ExpandedPluginsV1beta3.PreFilter,
					Filter:     defaults.ExpandedPluginsV1beta3.Filter,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1beta3.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: loadvariationriskbalancing.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - LowRiskOverCommitment with args",
			flags:           []string{"--config", lowRiskOverCommitmentConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(lowriskovercommitment.Name, lowriskovercommitment.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter:  defaults.ExpandedPluginsV1beta3.PreFilter,
					Filter:     defaults.ExpandedPluginsV1beta3.Filter,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					PreScore:   config.PluginSet{Enabled: []config.Plugin{{Name: lowriskovercommitment.Name}}},
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: lowriskovercommitment.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - NodeResourceTopologyMatch with args",
			flags:           []string{"--config", nodeResourceTopologyMatchConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(noderesourcetopology.Name, noderesourcetopology.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1.QueueSort,
					Bind:       defaults.ExpandedPluginsV1.Bind,
					PreFilter:  defaults.ExpandedPluginsV1.PreFilter,
					Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: noderesourcetopology.Name}}},
					PostFilter: defaults.ExpandedPluginsV1.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: noderesourcetopology.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1.Reserve,
					PreBind:    defaults.ExpandedPluginsV1.PreBind,
				},
			},
		},
		{
			name:            "single profile config - topologicalSort",
			flags:           []string{"--config", topologicalSortConfigFile},
			registryOptions: []app.Option{app.WithPlugin(topologicalsort.Name, topologicalsort.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  config.PluginSet{Enabled: []config.Plugin{{Name: topologicalsort.Name}}},
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		{
			name:            "single profile config - NetworkOverhead with args",
			flags:           []string{"--config", networkOverheadConfigWithArgsFile},
			registryOptions: []app.Option{app.WithPlugin(networkoverhead.Name, networkoverhead.New)},
			wantPlugins: map[string]*config.Plugins{
				"default-scheduler": {
					PreEnqueue: defaults.ExpandedPluginsV1beta3.PreEnqueue,
					QueueSort:  defaults.ExpandedPluginsV1beta3.QueueSort,
					Bind:       defaults.ExpandedPluginsV1beta3.Bind,
					PreFilter: config.PluginSet{
						Enabled: append(defaults.ExpandedPluginsV1beta3.PreFilter.Enabled, config.Plugin{Name: networkoverhead.Name}),
					},
					Filter:     config.PluginSet{Enabled: []config.Plugin{{Name: networkoverhead.Name}}},
					PostFilter: defaults.ExpandedPluginsV1beta3.PostFilter,
					PreScore:   defaults.ExpandedPluginsV1beta3.PreScore,
					Score:      config.PluginSet{Enabled: []config.Plugin{{Name: networkoverhead.Name, Weight: 1}}},
					Reserve:    defaults.ExpandedPluginsV1beta3.Reserve,
					PreBind:    defaults.ExpandedPluginsV1beta3.PreBind,
				},
			},
		},
		// TODO: add a multi profile test.
		// Ref: test "plugin config with multiple profiles" in
		// https://github.com/kubernetes/kubernetes/blob/master/cmd/kube-scheduler/app/server_test.go
	}

	makeListener := func(t *testing.T) net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("test", pflag.PanicOnError)
			opts := options.NewOptions()

			nfs := opts.Flags
			for _, f := range nfs.FlagSets {
				fs.AddFlagSet(f)
			}
			if err := fs.Parse(tc.flags); err != nil {
				t.Fatal(err)
			}

			// use listeners instead of static ports so parallel test runs don't conflict
			opts.SecureServing.Listener = makeListener(t)
			defer opts.SecureServing.Listener.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, sched, err := app.Setup(ctx, opts, tc.registryOptions...)
			if err != nil {
				t.Fatal(err)
			}

			gotPlugins := make(map[string]*config.Plugins)
			for n, p := range sched.Profiles {
				gotPlugins[n] = p.ListPlugins()
			}

			if diff := cmp.Diff(tc.wantPlugins, gotPlugins); diff != "" {
				t.Errorf("unexpected plugins diff (-want, +got): %s", diff)
			}
		})
	}
}
