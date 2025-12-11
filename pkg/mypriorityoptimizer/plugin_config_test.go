// plugin_config_test.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildPluginConfigSnapshot_BasicFields(t *testing.T) {
	snap := buildPluginConfigSnapshot()
	if snap.Name != Name {
		t.Fatalf("expected Name=%q in snapshot, got %q", Name, snap.Name)
	}
	if snap.Version != PluginVersion {
		t.Fatalf("expected Version=%q in snapshot, got %q", PluginVersion, snap.Version)
	}
	if snap.SystemNamespace != SystemNamespace {
		t.Fatalf("expected SystemNamespace=%q in snapshot, got %q", SystemNamespace, snap.SystemNamespace)
	}
	if snap.OptimizeMode == "" {
		t.Fatalf("expected non-empty OptimizeMode in snapshot")
	}
}

func TestPersistPluginConfig_CreatesAndUpdatesConfigMap(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	pl := &SharedState{
		Client:             client,
		BlockedWhileActive: newSafePodSet("test"),
	}

	// First call should create the ConfigMap.
	if err := pl.persistPluginConfig(ctx); err != nil {
		t.Fatalf("persistPluginConfig(create) error: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, PluginCfgConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create failed: %v", err)
	}
	if cm.Labels[PluginCfgConfigMapLabelKey] != "true" {
		t.Fatalf("expected label %q=true, got %v", PluginCfgConfigMapLabelKey, cm.Labels[PluginCfgConfigMapLabelKey])
	}

	raw, ok := cm.Data[PluginCfgConfigMapLabelKey+".json"]
	if !ok || raw == "" {
		t.Fatalf("expected data key %q to be present and non-empty", PluginCfgConfigMapLabelKey+".json")
	}

	var snap PluginConfigSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal snapshot failed: %v", err)
	}
	if snap.Name != Name {
		t.Fatalf("expected snapshot.Name=%q, got %q", Name, snap.Name)
	}

	// Second call should update in-place (still exactly one CM with that name).
	if err := pl.persistPluginConfig(ctx); err != nil {
		t.Fatalf("persistPluginConfig(update) error: %v", err)
	}
	list, err := client.CoreV1().ConfigMaps(SystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List after update failed: %v", err)
	}
	found := 0
	for _, c := range list.Items {
		if c.Name == PluginCfgConfigMapName {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly 1 ConfigMap named %q, found %d", PluginCfgConfigMapName, found)
	}
}
