// config_map_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// -------------------------
// Helpers
// -------------------------

// newConfigMapLister builds a lister func(ns string) ConfigMapNamespaceLister
// from a set of ConfigMaps.
func newConfigMapLister(cms ...*v1.ConfigMap) func(ns string) corev1listers.ConfigMapNamespaceLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	for _, cm := range cms {
		_ = indexer.Add(cm)
	}
	l := corev1listers.NewConfigMapLister(indexer)
	return func(ns string) corev1listers.ConfigMapNamespaceLister {
		return l.ConfigMaps(ns)
	}
}

// -------------------------
// listConfigMaps
// -------------------------

func TestListConfigMaps_SortsAndFilters(t *testing.T) {
	namespace := "ns6"
	labelKey := "myx/keep"

	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-1 * time.Hour)
	t3 := time.Now()

	cmOld := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-old",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t1),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	cmMid := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-mid",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t2),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	cmNew := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-new",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t3),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	// This one has no label -> should be ignored.
	cmNoLabel := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cm-nolabel",
			Namespace: namespace,
		},
	}

	lister := newConfigMapLister(cmOld, cmMid, cmNew, cmNoLabel)

	items, err := listConfigMaps(context.Background(), lister, namespace, labelKey)
	if err != nil {
		t.Fatalf("listConfigMaps error: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 labeled configmaps, got %d", len(items))
	}
	// Should be newest-first: cmNew, cmMid, cmOld
	if items[0].Name != "cm-new" || items[1].Name != "cm-mid" || items[2].Name != "cm-old" {
		t.Fatalf("unexpected order: %v, %v, %v", items[0].Name, items[1].Name, items[2].Name)
	}
}

// -----
// pruneConfigMaps
// -------------------------

func TestPruneConfigMaps(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		labelKey   string
		keep       int
		offsets    map[string]time.Duration
		wantExists map[string]bool
	}{
		{
			name:      "deletes oldest when keep=2",
			namespace: "ns7",
			labelKey:  "myx/prune",
			keep:      2,
			offsets: map[string]time.Duration{
				"cm1": -3 * time.Hour,
				"cm2": -2 * time.Hour,
				"cm3": -1 * time.Hour,
				"cm4": 0,
			},
			wantExists: map[string]bool{"cm4": true, "cm3": true, "cm2": false, "cm1": false},
		},
		{
			name:      "keep=0 is a no-op",
			namespace: "ns7-k0",
			labelKey:  "myx/prune-k0",
			keep:      0,
			offsets: map[string]time.Duration{
				"cm1": -1 * time.Hour,
				"cm2": 0,
			},
			wantExists: map[string]bool{"cm1": true, "cm2": true},
		},
		{
			name:      "keep>len(items) is a no-op",
			namespace: "ns7-kbig",
			labelKey:  "myx/prune-kbig",
			keep:      10,
			offsets: map[string]time.Duration{
				"cm1": -3 * time.Hour,
				"cm2": -2 * time.Hour,
				"cm3": -1 * time.Hour,
			},
			wantExists: map[string]bool{"cm1": true, "cm2": true, "cm3": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()

			var objs []runtime.Object
			var cms []*v1.ConfigMap
			for name, offset := range tt.offsets {
				cm := &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:              name,
						Namespace:         tt.namespace,
						Labels:            map[string]string{tt.labelKey: "true"},
						CreationTimestamp: metav1.NewTime(now.Add(offset)),
					},
				}
				objs = append(objs, cm)
				cms = append(cms, cm)
			}

			cli := fake.NewSimpleClientset(objs...)
			lister := newConfigMapLister(cms...)

			if err := pruneConfigMaps(ctx, cli.CoreV1(), lister, tt.namespace, tt.labelKey, tt.keep); err != nil {
				t.Fatalf("pruneConfigMaps failed: %v", err)
			}

			for name, wantExist := range tt.wantExists {
				_, err := getConfigMapByName(ctx, cli.CoreV1(), tt.namespace, name)
				if wantExist && err != nil {
					t.Errorf("expected %s to exist, got error: %v", name, err)
				}
				if !wantExist && err == nil {
					t.Errorf("expected %s to be deleted, but Get succeeded", name)
				}
			}
		})
	}
}

// -------------------------
// marshalJsonIndented
// -------------------------

func TestMarshalJsonIndented_Success(t *testing.T) {
	type payload struct {
		Foo string `json:"foo"`
		Bar int    `json:"bar"`
	}

	b, err := marshalJsonIndented(payload{Foo: "x", Bar: 42})
	if err != nil {
		t.Fatalf("marshalJsonIndented returned error: %v", err)
	}

	// Should be valid JSON with the expected fields.
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal of marshalled JSON failed: %v", err)
	}
	if got["foo"] != "x" {
		t.Fatalf(`expected foo="x", got %v`, got["foo"])
	}
	if v, ok := got["bar"].(float64); !ok || v != 42 {
		t.Fatalf("expected bar=42, got %T %v", got["bar"], got["bar"])
	}

	// And it should be pretty-printed (indented).
	s := string(b)
	if !strings.Contains(s, "\n") {
		t.Fatalf("expected pretty-printed JSON to contain a newline, got %q", s)
	}
	if !strings.Contains(s, "  \"foo\"") {
		t.Fatalf("expected pretty-printed JSON to contain indented key, got %q", s)
	}
}

func TestMarshalJsonIndented_Error(t *testing.T) {
	// json.MarshalIndent should fail on unsupported types (e.g. channel).
	_, err := marshalJsonIndented(make(chan int))
	if err == nil {
		t.Fatalf("expected error for unsupported type, got nil")
	}
}

// -------------------------
// jsonString
// -------------------------

func TestJsonString_Success(t *testing.T) {
	type payload struct {
		Answer int `json:"answer"`
	}

	s, err := jsonString(payload{Answer: 1234})
	if err != nil {
		t.Fatalf("jsonString returned error: %v", err)
	}

	var got map[string]int
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("unmarshal of jsonString output failed: %v", err)
	}
	if got["answer"] != 1234 {
		t.Fatalf("expected answer=1234, got %d", got["answer"])
	}
}

func TestJsonString_Error(t *testing.T) {
	_, err := jsonString(make(chan int))
	if err == nil {
		t.Fatalf("expected error for unsupported type, got nil")
	}
}

// -------------------------
// patchDataString
// -------------------------

func TestPatchDataString_UpdatesSingleKey(t *testing.T) {
	ctx := context.Background()
	namespace := "ns-patch"
	name := "cm-patch"
	dataKey := "myx/plan.json"

	// Seed fake client with a ConfigMap that already exists and has an extra key.
	cmInitial := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			dataKey: "old-value",
			"other": "keep-me",
		},
	}
	cli := fake.NewSimpleClientset(cmInitial)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		DataKey:   dataKey,
	}

	raw := `{"foo":"bar"}` // new value for dataKey
	if err := doc.patchDataString(ctx, cli.CoreV1(), raw); err != nil {
		t.Fatalf("patchDataString failed: %v", err)
	}

	// Verify that only dataKey was updated.
	cm, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after patchDataString failed: %v", err)
	}

	if got := cm.Data[dataKey]; got != raw {
		t.Fatalf("dataKey %q mismatch: got %q, want %q", dataKey, got, raw)
	}
	if got := cm.Data["other"]; got != "keep-me" {
		t.Fatalf("expected other key to be unchanged, got %q", got)
	}
}

// -------------------------
// ensureJson
// -------------------------

func TestEnsureJson_CreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	namespace := "ns1"
	name := "cm1"
	labelKey := "myx/plan"
	dataKey := "myx/plan.json"

	cli := fake.NewSimpleClientset()

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}

	type payload struct {
		Value string `json:"value"`
	}

	// 1) Create: configmap does not exist yet.
	if err := doc.ensureJson(ctx, cli.CoreV1(), payload{Value: "first"}); err != nil {
		t.Fatalf("ensureJson(create) failed: %v", err)
	}

	cm, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after create failed: %v", err)
	}
	if cm.Labels[labelKey] != "true" {
		t.Fatalf("expected label %q=true, got %v", labelKey, cm.Labels)
	}
	raw := cm.Data[dataKey]
	var got payload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal created data failed: %v", err)
	}
	if got.Value != "first" {
		t.Fatalf("expected value=first, got %q", got.Value)
	}

	// 2) Update: CM exists, call ensureJson again with new payload.
	if err := doc.ensureJson(ctx, cli.CoreV1(), payload{Value: "second"}); err != nil {
		t.Fatalf("ensureJson(update) failed: %v", err)
	}
	cm, err = getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after update failed: %v", err)
	}
	raw = cm.Data[dataKey]
	got = payload{}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal updated data failed: %v", err)
	}
	if got.Value != "second" {
		t.Fatalf("expected value=second after update, got %q", got.Value)
	}
}

func TestEnsureJson_UpdateOnNilData(t *testing.T) {
	ctx := context.Background()
	namespace := "ns1-nildata"
	name := "cm-nildata"
	labelKey := "myx/plan"
	dataKey := "myx/plan.json"

	// Existing CM with nil Data
	existing := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: nil,
	}
	cli := fake.NewSimpleClientset(existing)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}

	type payload struct {
		Value string `json:"value"`
	}

	if err := doc.ensureJson(ctx, cli.CoreV1(), payload{Value: "from-nil"}); err != nil {
		t.Fatalf("ensureJson(update on nil Data) failed: %v", err)
	}

	cm, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after ensureJson failed: %v", err)
	}
	if cm.Data == nil {
		t.Fatalf("expected Data map to be initialized, got nil")
	}

	raw := cm.Data[dataKey]
	var got payload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal updated data failed: %v", err)
	}
	if got.Value != "from-nil" {
		t.Fatalf("expected value=from-nil, got %q", got.Value)
	}
}

// -------------------------
// patchJson
// -------------------------

func TestPatchJson(t *testing.T) {
	ctx := context.Background()
	namespace := "ns2"
	name := "cm2"
	labelKey := "myx/plan"
	dataKey := "myx/plan.json"

	// Seed fake client with an existing ConfigMap
	initial := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: map[string]string{
			dataKey: `{"value":"old"}`,
		},
	}
	cli := fake.NewSimpleClientset(initial)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}
	type payload struct {
		Value string `json:"value"`
	}

	// Patch should replace only dataKey with new JSON.
	if err := doc.patchJson(ctx, cli.CoreV1(), payload{Value: "patched"}); err != nil {
		t.Fatalf("patchJson failed: %v", err)
	}

	cm, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after patch failed: %v", err)
	}
	raw := cm.Data[dataKey]
	var got payload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal patched data failed: %v", err)
	}
	if got.Value != "patched" {
		t.Fatalf("expected Value=patched, got %q", got.Value)
	}
}

func TestPatchJson_ErrorOnMarshalFailure(t *testing.T) {
	ctx := context.Background()
	namespace := "ns2-err"
	name := "cm2-err"
	labelKey := "myx/plan"
	dataKey := "myx/plan.json"

	initial := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: map[string]string{
			dataKey: `{"value":"old"}`,
		},
	}
	cli := fake.NewSimpleClientset(initial)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}

	// Channel cannot be marshalled to JSON -> force jsonString to fail.
	badValue := make(chan int)

	if err := doc.patchJson(ctx, cli.CoreV1(), badValue); err == nil {
		t.Fatalf("expected error from patchJson when marshalling fails, got nil")
	}

	// Ensure we did not touch the existing data.
	cm, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after patchJson error failed: %v", err)
	}
	if got := cm.Data[dataKey]; got != `{"value":"old"}` {
		t.Fatalf("expected dataKey to remain unchanged, got %q", got)
	}
}

// -------------------------
// readJson
// -------------------------

func TestReadJson_MissingAndPresent(t *testing.T) {
	namespace := "ns3"
	name := "cm3"
	dataKey := "myx/plan.json"

	// Lister with no items -> readJson returns (nil, nil)
	listerEmpty := newConfigMapLister()
	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		DataKey:   dataKey,
	}
	raw, err := doc.readJson(listerEmpty)
	if err != nil {
		t.Fatalf("readJson(empty) returned error: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil raw for missing CM, got %q", string(raw))
	}

	// Lister with a CM that has DataKey
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			dataKey: `{"hello":"world"}`,
		},
	}
	lister := newConfigMapLister(cm)
	raw, err = doc.readJson(lister)
	if err != nil {
		t.Fatalf("readJson(present) error: %v", err)
	}
	if string(raw) != cm.Data[dataKey] {
		t.Fatalf("readJson mismatch: got %q, want %q", string(raw), cm.Data[dataKey])
	}

	// sanity: IsNotFound should still be treated as "missing -> nil"
	_, notFound := lister(namespace).Get("does-not-exist") // just to show behavior
	if !apierrors.IsNotFound(notFound) {
		t.Fatalf("expected IsNotFound for missing name")
	}
}

// -------------------------
// mutateJson
// -------------------------

func TestMutateJson_Appends(t *testing.T) {
	ctx := context.Background()
	namespace := "ns4"
	name := "cm4"
	labelKey := "myx/arr"
	dataKey := "myx/arr.json"

	type item struct {
		ID int `json:"id"`
	}

	initialArr := []item{{ID: 1}, {ID: 2}}
	initialJSON, _ := json.Marshal(initialArr)

	// Seed CM with an array JSON at DataKey
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: map[string]string{
			dataKey: string(initialJSON),
		},
	}
	cli := fake.NewSimpleClientset(cm)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}
	lister := newConfigMapLister(cm)

	// mutateJson should load, call f(existing), and patch back.
	err := mutateJson(ctx, cli.CoreV1(), lister, doc, func(existing []item) ([]item, error) {
		return append(existing, item{ID: 3}), nil
	})
	if err != nil {
		t.Fatalf("mutateJson failed: %v", err)
	}

	updated, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after mutateJson failed: %v", err)
	}

	raw := updated.Data[dataKey]
	var arr []item
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		t.Fatalf("unmarshal after mutateJson failed: %v", err)
	}
	if len(arr) != 3 || arr[2].ID != 3 {
		t.Fatalf("expected appended item with ID=3, got %#v", arr)
	}
}

func TestMutateJson_MutateError(t *testing.T) {
	ctx := context.Background()
	namespace := "ns4-err"
	name := "cm4-err"
	labelKey := "myx/arr"
	dataKey := "myx/arr.json"

	type item struct {
		ID int `json:"id"`
	}

	initialArr := []item{{ID: 1}}
	initialJSON, _ := json.Marshal(initialArr)

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: map[string]string{
			dataKey: string(initialJSON),
		},
	}
	cli := fake.NewSimpleClientset(cm)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		LabelKey:  labelKey,
		DataKey:   dataKey,
	}
	lister := newConfigMapLister(cm)

	err := mutateJson(ctx, cli.CoreV1(), lister, doc, func(existing []item) ([]item, error) {
		if len(existing) != 1 || existing[0].ID != 1 {
			t.Fatalf("unexpected existing slice in mutateJson: %#v", existing)
		}
		return nil, fmt.Errorf("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected mutateJson to return our error, got %v", err)
	}

	// Verify that the ConfigMap was not changed on error
	updated, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after mutateJson error failed: %v", err)
	}
	if updated.Data[dataKey] != string(initialJSON) {
		t.Fatalf("expected dataKey to remain unchanged on error, got %q", updated.Data[dataKey])
	}
}

// -------------------------
// mutateRaw
// -------------------------

func TestMutateRaw_Uppercases(t *testing.T) {
	ctx := context.Background()
	namespace := "ns5"
	name := "cm5"
	dataKey := "myx/raw.json"

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			dataKey: `{"msg":"hello"}`,
		},
	}
	cli := fake.NewSimpleClientset(cm)
	lister := newConfigMapLister(cm)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		DataKey:   dataKey,
	}

	err := doc.mutateRaw(ctx, cli.CoreV1(), lister, func(raw []byte) ([]byte, error) {
		// very silly transformation: replace "hello" with "HELLO"
		s2 := `{"msg":"HELLO"}`
		return []byte(s2), nil
	})
	if err != nil {
		t.Fatalf("mutateRaw failed: %v", err)
	}

	updated, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after mutateRaw failed: %v", err)
	}
	if updated.Data[dataKey] != `{"msg":"HELLO"}` {
		t.Fatalf("mutateRaw did not update dataKey; got %q", updated.Data[dataKey])
	}
}

func TestMutateRaw_MissingConfigMap_NoOp(t *testing.T) {
	ctx := context.Background()
	namespace := "ns5-miss"
	name := "cm5-miss"
	dataKey := "myx/raw.json"

	cli := fake.NewSimpleClientset()
	lister := newConfigMapLister() // no items

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		DataKey:   dataKey,
	}

	called := false
	err := doc.mutateRaw(ctx, cli.CoreV1(), lister, func(raw []byte) ([]byte, error) {
		called = true
		return raw, nil
	})
	if err != nil {
		t.Fatalf("mutateRaw on missing CM returned error: %v", err)
	}
	if called {
		t.Fatalf("mutate callback should not be called when CM/data is missing")
	}

	// Still no ConfigMap created.
	if _, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name); err == nil {
		t.Fatalf("expected no ConfigMap to be created on missing mutateRaw")
	}
}

func TestMutateRaw_NilNewRaw_NoOp(t *testing.T) {
	ctx := context.Background()
	namespace := "ns5-nilraw"
	name := "cm5-nilraw"
	dataKey := "myx/raw.json"

	original := `{"msg":"hello"}`
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			dataKey: original,
		},
	}
	cli := fake.NewSimpleClientset(cm)
	lister := newConfigMapLister(cm)

	doc := ConfigMapDoc{
		Namespace: namespace,
		Name:      name,
		DataKey:   dataKey,
	}

	calls := 0
	err := doc.mutateRaw(ctx, cli.CoreV1(), lister, func(raw []byte) ([]byte, error) {
		calls++
		if string(raw) != original {
			t.Fatalf("unexpected raw in mutate: %q", string(raw))
		}
		// Signal "no change" by returning nil, nil.
		return nil, nil
	})
	if err != nil {
		t.Fatalf("mutateRaw returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected mutate to be called once, got %d", calls)
	}

	updated, err := getConfigMapByName(ctx, cli.CoreV1(), namespace, name)
	if err != nil {
		t.Fatalf("Get after mutateRaw no-op failed: %v", err)
	}
	if updated.Data[dataKey] != original {
		t.Fatalf("expected dataKey to remain unchanged, got %q", updated.Data[dataKey])
	}
}
