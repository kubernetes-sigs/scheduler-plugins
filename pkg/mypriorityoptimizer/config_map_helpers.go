// config_map_helpers.go
package mypriorityoptimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// -------------------------
// listConfigMaps
// -------------------------

// List config maps by label newest-first.
func listConfigMaps(
	_ context.Context,
	nsLister corev1listers.ConfigMapNamespaceLister,
	labelKey string,
) ([]apiv1.ConfigMap, error) {
	sel := labels.SelectorFromSet(labels.Set{labelKey: "true"})
	items, err := nsLister.List(sel)
	if err != nil {
		return nil, err
	}
	cms := make([]apiv1.ConfigMap, len(items))
	for i := range items {
		cms[i] = *items[i].DeepCopy()
	}
	sort.Slice(cms, func(i, j int) bool {
		return cms[i].CreationTimestamp.Time.After(cms[j].CreationTimestamp.Time)
	})
	return cms, nil
}

// --------------------------
// getConfigMapByName
// -------------------------

// getConfigMapByName retrieves a config map by name.
func getConfigMapByName(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	namespace,
	name string,
) (*apiv1.ConfigMap, error) {
	return cli.ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}

// -------------------------
// pruneConfigMaps
// -------------------------

// Keep first K newest config maps with label, delete the rest.
func pruneConfigMaps(
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	nsLister corev1listers.ConfigMapNamespaceLister,
	labelKey string,
	keep int,
) error {
	if keep <= 0 {
		return nil
	}
	items, err := listConfigMaps(ctx, nsLister, labelKey)
	if err != nil || len(items) <= keep {
		return err
	}
	for i := keep; i < len(items); i++ {
		_ = cms.Delete(ctx, items[i].Name, metav1.DeleteOptions{})
	}
	return nil
}

// -------------------------
// marshalJsonIndented
// -------------------------

// marshalJsonIndented marshals an object to JSON with indentation.
func marshalJsonIndented(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// -------------------------
// jsonString
// -------------------------

// jsonString pretty-prints v to JSON and returns it as a string.
func jsonString(v any) (string, error) {
	b, err := marshalJsonIndented(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// -------------------------
// patchDataString
// -------------------------

// patchDataString patches a single DataKey with the given raw JSON string.
func (d ConfigMapDoc) patchDataString(
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	raw string,
) error {
	patch := []byte(fmt.Sprintf(`{"data":{"%s":%q}}`, d.DataKey, raw))
	_, err := cms.Patch(
		ctx,
		d.Name,
		types.MergePatchType,
		patch,
		metav1.PatchOptions{},
	)
	return err
}

// -------------------------
// ensureJson
// -------------------------

// Create or update config map, storing data as JSON at DataKey.
func (d ConfigMapDoc) ensureJson(
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	data any,
) error {
	b, err := marshalJsonIndented(data)
	if err != nil {
		return err
	}

	cm, err := cms.Get(ctx, d.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		cm = &apiv1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      d.Name,
				Namespace: d.Namespace,
				Labels:    map[string]string{d.LabelKey: "true"},
			},
			Data: map[string]string{d.DataKey: string(b)},
		}
		_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
		return err

	case err != nil:
		return err

	default:
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[d.DataKey] = string(b)
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}
}

// -------------------------
// patchJson
// -------------------------

// Patch only DataKey via merge patch.
func (d ConfigMapDoc) patchJson(
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	v any,
) error {
	jsonStr, err := jsonString(v)
	if err != nil {
		return err
	}
	return d.patchDataString(ctx, cms, jsonStr)
}

// -------------------------
// readJson
// -------------------------

// readJson reads DataKey as JSON bytes.
// readJson reads DataKey as JSON bytes.
func (d ConfigMapDoc) readJson(
	nsLister corev1listers.ConfigMapNamespaceLister,
) (raw []byte, found bool, err error) {
	cm, err := nsLister.Get(d.Name)
	// NotFound => treat as missing (no error)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if cm == nil {
		return nil, false, nil
	}

	return []byte(cm.Data[d.DataKey]), true, nil
}

// -------------------------
// mutateJson
// -------------------------

// mutateJson loads -> mutates -> patches an array JSON.
func mutateJson[T any](
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	nsLister corev1listers.ConfigMapNamespaceLister,
	doc ConfigMapDoc,
	f func(existing []T) ([]T, error),
) error {
	raw, found, err := doc.readJson(nsLister)
	if err != nil || !found {
		return err // no-op on missing, propagate error
	}

	var arr []T
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &arr)
	}

	out, err := f(arr)
	if err != nil || out == nil { // allow nil => “no change”
		return err
	}
	return doc.patchJson(ctx, cms, out)
}

// -------------------------
// mutateRaw
// -------------------------

// mutateRaw loads the JSON string at DataKey, mutates it, and writes the result
// back.
func (d ConfigMapDoc) mutateRaw(
	ctx context.Context,
	cms corev1client.ConfigMapInterface,
	nsLister corev1listers.ConfigMapNamespaceLister,
	mutate func(raw []byte) ([]byte, error),
) error {
	raw, found, err := d.readJson(nsLister)
	if err != nil || !found {
		return err // missing => no-op
	}
	newRaw, err := mutate(raw)
	if err != nil || newRaw == nil {
		return err // nil => no-op
	}
	return d.patchDataString(ctx, cms, string(newRaw))
}
