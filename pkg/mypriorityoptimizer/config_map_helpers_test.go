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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// -------------------------
// Test Helpers
// -------------------------

// cmNSLister is a stub that lets tests inject List/Get behaviors without wiring
// informers/indexers.
type cmNSLister struct {
	listFn func() ([]*v1.ConfigMap, error)
	getFn  func(name string) (*v1.ConfigMap, error)
}

func (c cmNSLister) List(_ labels.Selector) ([]*v1.ConfigMap, error) { return c.listFn() }

func (c cmNSLister) Get(name string) (*v1.ConfigMap, error) { return c.getFn(name) }

// nsLister returns a namespace-scoped lister backed by an in-memory indexer.
func nsLister(ns string, cms ...*v1.ConfigMap) corev1listers.ConfigMapNamespaceLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	for _, cm := range cms {
		if cm == nil {
			continue
		}
		_ = indexer.Add(cm)
	}
	return corev1listers.NewConfigMapLister(indexer).ConfigMaps(ns)
}

// makeCm is a helper to create a ConfigMap with given params.
func makeCm(ns, name string, lbls map[string]string, data map[string]string, ts time.Time) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			Labels:            lbls,
			CreationTimestamp: metav1.NewTime(ts),
		},
		Data: data,
	}
}

// makeCmDoc is a helper to create a ConfigMapDoc with given params.
func makeCmDoc(ns, name, labelKey, dataKey string) ConfigMapDoc {
	return ConfigMapDoc{Namespace: ns, Name: name, LabelKey: labelKey, DataKey: dataKey}
}

func setupCm(
	ns string,
	cm *v1.ConfigMap,
) (*fake.Clientset, corev1client.ConfigMapInterface, corev1listers.ConfigMapNamespaceLister) {
	var objs []runtime.Object
	if cm != nil {
		objs = append(objs, cm)
	}
	cli := fake.NewSimpleClientset(objs...)
	var lister corev1listers.ConfigMapNamespaceLister
	if cm != nil {
		lister = nsLister(ns, cm)
	} else {
		lister = nsLister(ns)
	}
	return cli, cli.CoreV1().ConfigMaps(ns), lister
}

// -------------------------
// listConfigMaps
// -------------------------

func TestListConfigMaps_SortsAndFilters(t *testing.T) {
	ctx := context.Background()
	namespace := "ns6"
	labelKey := "myx/keep"
	now := time.Now()

	cmOld := makeCm(namespace, "cm-old", map[string]string{labelKey: "true"}, nil, now.Add(-2*time.Hour))
	cmMid := makeCm(namespace, "cm-mid", map[string]string{labelKey: "true"}, nil, now.Add(-1*time.Hour))
	cmNew := makeCm(namespace, "cm-new", map[string]string{labelKey: "true"}, nil, now)
	cmNoLabel := makeCm(namespace, "cm-nolabel", nil, nil, now.Add(-30*time.Minute)) // ignored

	lister := nsLister(namespace, cmOld, cmMid, cmNew, cmNoLabel)

	items, err := listConfigMaps(ctx, lister, labelKey)
	if err != nil {
		t.Fatalf("listConfigMaps error: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 labeled configmaps, got %d", len(items))
	}
	// Newest first: cmNew, cmMid, cmOld
	if items[0].Name != "cm-new" || items[1].Name != "cm-mid" || items[2].Name != "cm-old" {
		t.Fatalf("unexpected order: %v, %v, %v", items[0].Name, items[1].Name, items[2].Name)
	}
}

func TestListConfigMaps_ListError(t *testing.T) {
	wantErr := fmt.Errorf("boom")
	lister := cmNSLister{
		listFn: func() ([]*v1.ConfigMap, error) { return nil, wantErr },
		getFn:  func(string) (*v1.ConfigMap, error) { t.Fatal("unexpected Get"); return nil, nil },
	}

	_, err := listConfigMaps(context.Background(), lister, "label")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected list error, got %v", err)
	}
}

// -------------------------
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
			var seeded []*v1.ConfigMap
			for name, offset := range tt.offsets {
				cm := makeCm(tt.namespace, name, map[string]string{tt.labelKey: "true"}, nil, now.Add(offset))
				objs = append(objs, cm)
				seeded = append(seeded, cm)
			}

			cli := fake.NewSimpleClientset(objs...)
			cms := cli.CoreV1().ConfigMaps(tt.namespace)
			lister := nsLister(tt.namespace, seeded...)

			if err := pruneConfigMaps(ctx, cms, lister, tt.labelKey, tt.keep); err != nil {
				t.Fatalf("pruneConfigMaps failed: %v", err)
			}

			for name, wantExist := range tt.wantExists {
				_, err := cms.Get(ctx, name, metav1.GetOptions{})
				switch {
				case wantExist && err != nil:
					t.Errorf("expected %s to exist, got error: %v", name, err)
				case !wantExist && err == nil:
					t.Errorf("expected %s to be deleted, but Get succeeded", name)
				case !wantExist && err != nil && !apierrors.IsNotFound(err):
					t.Errorf("expected notfound for %s, got: %v", name, err)
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

	// Pretty-printed.
	s := string(b)
	if !strings.Contains(s, "\n") {
		t.Fatalf("expected pretty-printed JSON to contain a newline, got %q", s)
	}
	if !strings.Contains(s, "  \"foo\"") {
		t.Fatalf("expected pretty-printed JSON to contain indented key, got %q", s)
	}
}

func TestMarshalJsonIndented_Error(t *testing.T) {
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

	s, err := marshalToJsonString(payload{Answer: 1234})
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
	_, err := marshalToJsonString(make(chan int))
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

	initial := makeCm(namespace, name, nil, map[string]string{
		dataKey: "old-value",
		"other": "keep-me",
	}, time.Now())

	_, cms, _ := setupCm(namespace, initial)
	doc := makeCmDoc(namespace, name, "", dataKey)

	raw := `{"foo":"bar"}`
	if err := doc.patchDataString(ctx, cms, raw); err != nil {
		t.Fatalf("patchDataString failed: %v", err)
	}

	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
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

	_, cms, _ := setupCm(namespace, nil)
	doc := makeCmDoc(namespace, name, labelKey, dataKey)

	type payload struct {
		Value string `json:"value"`
	}

	// Create
	if err := doc.ensureJson(ctx, cms, payload{Value: "first"}); err != nil {
		t.Fatalf("ensureJson(create) failed: %v", err)
	}

	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create failed: %v", err)
	}
	if cm.Labels[labelKey] != "true" {
		t.Fatalf("expected label %q=true, got %v", labelKey, cm.Labels)
	}
	var got payload
	if err := json.Unmarshal([]byte(cm.Data[dataKey]), &got); err != nil {
		t.Fatalf("unmarshal created data failed: %v", err)
	}
	if got.Value != "first" {
		t.Fatalf("expected value=first, got %q", got.Value)
	}

	// Update
	if err := doc.ensureJson(ctx, cms, payload{Value: "second"}); err != nil {
		t.Fatalf("ensureJson(update) failed: %v", err)
	}
	cm, err = cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after update failed: %v", err)
	}
	got = payload{}
	if err := json.Unmarshal([]byte(cm.Data[dataKey]), &got); err != nil {
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

	existing := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{labelKey: "true"},
		},
		Data: nil,
	}

	_, cms, _ := setupCm(namespace, existing)
	doc := makeCmDoc(namespace, name, labelKey, dataKey)

	type payload struct {
		Value string `json:"value"`
	}

	if err := doc.ensureJson(ctx, cms, payload{Value: "from-nil"}); err != nil {
		t.Fatalf("ensureJson(update on nil Data) failed: %v", err)
	}

	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after ensureJson failed: %v", err)
	}
	if cm.Data == nil {
		t.Fatalf("expected Data map to be initialized, got nil")
	}

	var got payload
	if err := json.Unmarshal([]byte(cm.Data[dataKey]), &got); err != nil {
		t.Fatalf("unmarshal updated data failed: %v", err)
	}
	if got.Value != "from-nil" {
		t.Fatalf("expected value=from-nil, got %q", got.Value)
	}
}

func TestEnsureJson_PropagatesClientErrors(t *testing.T) {
	ctx := context.Background()

	type tc struct {
		name       string
		verb       string
		seed       *v1.ConfigMap
		wantSubstr string
	}

	tests := []tc{
		{
			name:       "get fails",
			verb:       "get",
			seed:       nil,
			wantSubstr: "get-fail",
		},
		{
			name:       "create fails",
			verb:       "create",
			seed:       nil,
			wantSubstr: "create-fail",
		},
		{
			name: "update fails",
			verb: "update",
			seed: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cm",
					Namespace: "ns",
					Labels:    map[string]string{"lk": "true"},
				},
				Data: map[string]string{"dk": `{"old":true}`},
			},
			wantSubstr: "update-fail",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cli *fake.Clientset
			if tt.seed != nil {
				cli = fake.NewSimpleClientset(tt.seed)
			} else {
				cli = fake.NewSimpleClientset()
			}

			cli.Fake.PrependReactor(tt.verb, "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("%s", tt.wantSubstr)
			})

			cms := cli.CoreV1().ConfigMaps("ns")
			doc := makeCmDoc("ns", "cm", "lk", "dk")

			err := doc.ensureJson(ctx, cms, map[string]any{"x": 1})
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantSubstr, err)
			}
		})
	}
}

func TestEnsureJson_MarshalError_NoClientActions(t *testing.T) {
	ctx := context.Background()
	cli, cms, _ := setupCm("ns", nil)
	doc := makeCmDoc("ns", "cm", "lk", "dk")

	err := doc.ensureJson(ctx, cms, make(chan int))
	if err == nil {
		t.Fatalf("expected marshal error, got nil")
	}
	if got := len(cli.Actions()); got != 0 {
		t.Fatalf("expected no client actions on marshal error, got %d: %#v", got, cli.Actions())
	}
}

// -------------------------
// readJson
// -------------------------

func TestReadJson_NilConfigMapNoError(t *testing.T) {
	doc := makeCmDoc("ns", "cm", "", "dk")

	lister := cmNSLister{
		getFn:  func(string) (*v1.ConfigMap, error) { return nil, nil }, // hit cm == nil branch
		listFn: func() ([]*v1.ConfigMap, error) { t.Fatal("unexpected List"); return nil, nil },
	}

	raw, found, err := doc.readJson(lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false when cm is nil")
	}
	if raw != nil {
		t.Fatalf("expected nil raw when cm is nil, got %q", string(raw))
	}
}

func TestReadJson_MissingAndPresent(t *testing.T) {
	namespace := "ns3"
	name := "cm3"
	dataKey := "myx/plan.json"

	doc := makeCmDoc(namespace, name, "", dataKey)

	// Missing
	raw, found, err := doc.readJson(nsLister(namespace))
	if err != nil {
		t.Fatalf("readJson(missing) returned error: %v", err)
	}
	if found || raw != nil {
		t.Fatalf("expected missing -> (nil,false,nil), got raw=%v found=%v err=%v", raw, found, err)
	}

	// Present
	cm := makeCm(namespace, name, nil, map[string]string{dataKey: `{"hello":"world"}`}, time.Now())
	raw, found, err = doc.readJson(nsLister(namespace, cm))
	if err != nil {
		t.Fatalf("readJson(present) error: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if string(raw) != cm.Data[dataKey] {
		t.Fatalf("readJson mismatch: got %q, want %q", string(raw), cm.Data[dataKey])
	}
}

func TestReadJson_GetReturnsError(t *testing.T) {
	doc := makeCmDoc("ns", "cm", "", "dk")

	lister := cmNSLister{
		getFn:  func(string) (*v1.ConfigMap, error) { return nil, fmt.Errorf("boom") },
		listFn: func() ([]*v1.ConfigMap, error) { t.Fatal("unexpected List"); return nil, nil },
	}

	_, _, err := doc.readJson(lister)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestReadJson_KeyMissing(t *testing.T) {
	doc := makeCmDoc("ns", "cm", "", "dk")
	cm := makeCm("ns", "cm", nil, map[string]string{}, time.Now())

	raw, found, err := doc.readJson(nsLister("ns", cm))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if raw == nil || string(raw) != "" {
		t.Fatalf("expected empty bytes, got %v (%q)", raw, string(raw))
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

	initial := makeCm(namespace, name, map[string]string{labelKey: "true"}, map[string]string{
		dataKey: `{"value":"old"}`,
	}, time.Now())

	_, cms, _ := setupCm(namespace, initial)
	doc := makeCmDoc(namespace, name, labelKey, dataKey)

	type payload struct {
		Value string `json:"value"`
	}

	if err := doc.patchJson(ctx, cms, payload{Value: "patched"}); err != nil {
		t.Fatalf("patchJson failed: %v", err)
	}

	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after patch failed: %v", err)
	}

	var got payload
	if err := json.Unmarshal([]byte(cm.Data[dataKey]), &got); err != nil {
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

	initial := makeCm(namespace, name, map[string]string{labelKey: "true"}, map[string]string{
		dataKey: `{"value":"old"}`,
	}, time.Now())

	_, cms, _ := setupCm(namespace, initial)
	doc := makeCmDoc(namespace, name, labelKey, dataKey)

	badValue := make(chan int)
	if err := doc.patchJson(ctx, cms, badValue); err == nil {
		t.Fatalf("expected error from patchJson when marshalling fails, got nil")
	}

	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after patchJson error failed: %v", err)
	}
	if got := cm.Data[dataKey]; got != `{"value":"old"}` {
		t.Fatalf("expected dataKey to remain unchanged, got %q", got)
	}
}

// -------------------------
// mutateJson
// -------------------------

func TestMutateJson_Appends(t *testing.T) {
	ctx := context.Background()
	namespace := "ns4"
	name := "cm4"
	dataKey := "myx/arr.json"

	type item struct {
		ID int `json:"id"`
	}

	initialArr := []item{{ID: 1}, {ID: 2}}
	initialJSON, _ := json.Marshal(initialArr)

	cm := makeCm(namespace, name, nil, map[string]string{
		dataKey: string(initialJSON),
	}, time.Now())

	_, cms, _ := setupCm(namespace, cm)
	doc := makeCmDoc(namespace, name, "", dataKey)
	lister := nsLister(namespace, cm)

	err := mutateJson(ctx, cms, lister, doc, func(existing []item) ([]item, error) {
		return append(existing, item{ID: 3}), nil
	})
	if err != nil {
		t.Fatalf("mutateJson failed: %v", err)
	}

	updated, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after mutateJson failed: %v", err)
	}

	var arr []item
	if err := json.Unmarshal([]byte(updated.Data[dataKey]), &arr); err != nil {
		t.Fatalf("unmarshal after mutateJson failed: %v", err)
	}
	if len(arr) != 3 || arr[2].ID != 3 {
		t.Fatalf("expected appended item with ID=3, got %#v", arr)
	}
}

func TestMutateJson_MutateError_NoChange(t *testing.T) {
	ctx := context.Background()
	namespace := "ns4-err"
	name := "cm4-err"
	dataKey := "myx/arr.json"

	type item struct {
		ID int `json:"id"`
	}

	initialArr := []item{{ID: 1}}
	initialJSON, _ := json.Marshal(initialArr)

	cm := makeCm(namespace, name, nil, map[string]string{
		dataKey: string(initialJSON),
	}, time.Now())

	_, cms, _ := setupCm(namespace, cm)
	doc := makeCmDoc(namespace, name, "", dataKey)
	lister := nsLister(namespace, cm)

	err := mutateJson(ctx, cms, lister, doc, func(existing []item) ([]item, error) {
		if len(existing) != 1 || existing[0].ID != 1 {
			t.Fatalf("unexpected existing slice in mutateJson: %#v", existing)
		}
		return nil, fmt.Errorf("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected mutateJson to return our error, got %v", err)
	}

	updated, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after mutateJson error failed: %v", err)
	}
	if updated.Data[dataKey] != string(initialJSON) {
		t.Fatalf("expected dataKey to remain unchanged on error, got %q", updated.Data[dataKey])
	}
}

func TestMutateJson_PropagatesReadError(t *testing.T) {
	ctx := context.Background()
	_, cms, _ := setupCm("ns", nil)
	doc := makeCmDoc("ns", "cm", "", "dk")
	lister := cmNSLister{
		getFn:  func(string) (*v1.ConfigMap, error) { return nil, fmt.Errorf("read-fail") },
		listFn: func() ([]*v1.ConfigMap, error) { return nil, nil },
	}

	called := false
	err := mutateJson(ctx, cms, lister, doc, func(_ []int) ([]int, error) {
		called = true
		return nil, nil
	})
	if called {
		t.Fatalf("mutate function should not run on read error")
	}
	if err == nil || !strings.Contains(err.Error(), "read-fail") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestMutateJson_PatchFails(t *testing.T) {
	ctx := context.Background()
	namespace := "ns"
	name := "cm"
	dataKey := "dk"

	cm := makeCm(namespace, name, nil, map[string]string{dataKey: `[1,2]`}, time.Now())
	cli, cms, _ := setupCm(namespace, cm)

	cli.Fake.PrependReactor("patch", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("patch-fail")
	})

	doc := makeCmDoc(namespace, name, "", dataKey)
	l := nsLister(namespace, cm)

	err := mutateJson(ctx, cms, l, doc, func(existing []int) ([]int, error) {
		return append(existing, 3), nil
	})
	if err == nil || !strings.Contains(err.Error(), "patch-fail") {
		t.Fatalf("expected patch error, got %v", err)
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

	cm := makeCm(namespace, name, nil, map[string]string{
		dataKey: `{"msg":"hello"}`,
	}, time.Now())

	_, cms, _ := setupCm(namespace, cm)
	doc := makeCmDoc(namespace, name, "", dataKey)
	l := nsLister(namespace, cm)

	err := doc.mutateRaw(ctx, cms, l, func(_ []byte) ([]byte, error) {
		return []byte(`{"msg":"HELLO"}`), nil
	})
	if err != nil {
		t.Fatalf("mutateRaw failed: %v", err)
	}

	updated, err := cms.Get(ctx, name, metav1.GetOptions{})
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

	_, cms, _ := setupCm(namespace, nil)
	doc := makeCmDoc(namespace, name, "", dataKey)
	lister := nsLister(namespace)

	called := false
	err := doc.mutateRaw(ctx, cms, lister, func(raw []byte) ([]byte, error) {
		called = true
		return raw, nil
	})
	if err != nil {
		t.Fatalf("mutateRaw on missing CM returned error: %v", err)
	}
	if called {
		t.Fatalf("mutate callback should not be called when CM is missing")
	}

	_, err = cms.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		t.Fatalf("expected no ConfigMap to be created on missing mutateRaw")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected notfound, got: %v", err)
	}
}

func TestMutateRaw_NilNewRaw_NoOp(t *testing.T) {
	ctx := context.Background()
	namespace := "ns5-nilraw"
	name := "cm5-nilraw"
	dataKey := "myx/raw.json"

	original := `{"msg":"hello"}`
	cm := makeCm(namespace, name, nil, map[string]string{
		dataKey: original,
	}, time.Now())

	_, cms, _ := setupCm(namespace, cm)
	doc := makeCmDoc(namespace, name, "", dataKey)
	lister := nsLister(namespace, cm)

	calls := 0
	err := doc.mutateRaw(ctx, cms, lister, func(raw []byte) ([]byte, error) {
		calls++
		if string(raw) != original {
			t.Fatalf("unexpected raw in mutate: %q", string(raw))
		}
		return nil, nil // "no change"
	})
	if err != nil {
		t.Fatalf("mutateRaw returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected mutate to be called once, got %d", calls)
	}

	updated, err := cms.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after mutateRaw no-op failed: %v", err)
	}
	if updated.Data[dataKey] != original {
		t.Fatalf("expected dataKey to remain unchanged, got %q", updated.Data[dataKey])
	}
}
