// config_map_helpers_test.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// -----------------------------------------------------------------------------
// marshalJSONIndented
// -----------------------------------------------------------------------------

// TODO: missing

// -----------------------------------------------------------------------------
// patchDataString
// -----------------------------------------------------------------------------

// TODO: missing

// -----------------------------------------------------------------------------
// ensureJson
// -----------------------------------------------------------------------------

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

	cm, err := cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
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
	cm, err = cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
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

// -----------------------------------------------------------------------------
// patchJson
// -----------------------------------------------------------------------------

func TestPatchJson(t *testing.T) {
	ctx := context.Background()
	namespace := "ns2"
	name := "cm2"
	labelKey := "myx/plan"
	dataKey := "myx/plan.json"

	// Seed fake client with an existing ConfigMap
	initial := &apiv1.ConfigMap{
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

	cm, err := cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
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

// -----------------------------------------------------------------------------
// readJson
// -----------------------------------------------------------------------------

func TestReadJson_MissingAndPresent(t *testing.T) {
	namespace := "ns3"
	name := "cm3"
	dataKey := "myx/plan.json"

	// Lister with no items → readJson returns (nil, nil)
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
	cm := &apiv1.ConfigMap{
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

// -----------------------------------------------------------------------------
// mutateJson
// -----------------------------------------------------------------------------

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
	cm := &apiv1.ConfigMap{
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

	updated, err := cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
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

// -----------------------------------------------------------------------------
// mutateJson
// -----------------------------------------------------------------------------

func TestMutateRaw_Uppercases(t *testing.T) {
	ctx := context.Background()
	namespace := "ns5"
	name := "cm5"
	dataKey := "myx/raw.json"

	cm := &apiv1.ConfigMap{
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

	updated, err := cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after mutateRaw failed: %v", err)
	}
	if updated.Data[dataKey] != `{"msg":"HELLO"}` {
		t.Fatalf("mutateRaw did not update dataKey; got %q", updated.Data[dataKey])
	}
}

// -----------------------------------------------------------------------------
// listConfigMaps
// -----------------------------------------------------------------------------

func TestListConfigMaps_SortsAndFilters(t *testing.T) {
	namespace := "ns6"
	labelKey := "myx/keep"

	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-1 * time.Hour)
	t3 := time.Now()

	cmOld := &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-old",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t1),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	cmMid := &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-mid",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t2),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	cmNew := &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cm-new",
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(t3),
			Labels:            map[string]string{labelKey: "true"},
		},
	}
	// This one has no label -> should be ignored.
	cmNoLabel := &apiv1.ConfigMap{
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

// -----------------------------------------------------------------------------
// pruneConfigMaps
// -----------------------------------------------------------------------------

func TestPruneConfigMaps_DeletesOldOnes(t *testing.T) {
	ctx := context.Background()
	namespace := "ns7"
	labelKey := "myx/prune"

	now := time.Now()
	makeCM := func(name string, offset time.Duration) *apiv1.ConfigMap {
		return &apiv1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         namespace,
				Labels:            map[string]string{labelKey: "true"},
				CreationTimestamp: metav1.NewTime(now.Add(offset)),
			},
		}
	}

	cm1 := makeCM("cm1", -3*time.Hour)
	cm2 := makeCM("cm2", -2*time.Hour)
	cm3 := makeCM("cm3", -1*time.Hour)
	cm4 := makeCM("cm4", 0)

	schemeObjs := []runtime.Object{cm1, cm2, cm3, cm4}
	cli := fake.NewSimpleClientset(schemeObjs...)

	// lister must see all four
	lister := newConfigMapLister(cm1, cm2, cm3, cm4)

	// Keep 2 newest; expect the two oldest to be deleted.
	if err := pruneConfigMaps(ctx, cli.CoreV1(), lister, namespace, labelKey, 2); err != nil {
		t.Fatalf("pruneConfigMaps failed: %v", err)
	}

	// cm4 & cm3 should remain; cm2 & cm1 should be gone.
	for name, wantExist := range map[string]bool{
		"cm4": true,
		"cm3": true,
		"cm2": false,
		"cm1": false,
	} {
		_, err := cli.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if wantExist && err != nil {
			t.Errorf("expected %s to exist, got error: %v", name, err)
		}
		if !wantExist && err == nil {
			t.Errorf("expected %s to be deleted, but Get succeeded", name)
		}
	}
}
