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
// --------------------------

// List config maps by label newest-first.
func listConfigMaps(
	_ context.Context,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	namespace,
	labelKey string,
) ([]apiv1.ConfigMap, error) {
	// Select config maps with the given label.
	sel := labels.SelectorFromSet(labels.Set{labelKey: "true"})
	items, err := lister(namespace).List(sel)

	if err != nil {
		return nil, err
	}

	// Make a copy to avoid mutating the informer cache.
	cms := make([]apiv1.ConfigMap, len(items))
	for i := range items {
		cms[i] = *items[i].DeepCopy()
	}

	// Sort by creation timestamp, newest first.
	sort.Slice(cms, func(i, j int) bool {
		return cms[i].CreationTimestamp.Time.After(cms[j].CreationTimestamp.Time)
	})
	return cms, nil
}

// ---------------------------
// getConfigMapByName
// --------------------------

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
// --------------------------

// Keep first K newest config maps with label, delete the rest.
func pruneConfigMaps(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	namespace, labelKey string,
	keep int,
) error {
	if keep <= 0 {
		return nil
	}
	items, err := listConfigMaps(ctx, lister, namespace, labelKey)
	if err != nil || len(items) <= keep {
		return err
	}
	cms := cli.ConfigMaps(namespace)
	for i := keep; i < len(items); i++ {
		_ = cms.Delete(ctx, items[i].Name, metav1.DeleteOptions{})
	}
	return nil
}

// -------------------------
// marshalJsonIndented
// --------------------------

// marshalJsonIndented marshals an object to JSON with indentation.
func marshalJsonIndented(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// -------------------------
// jsonString
// --------------------------

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
// --------------------------

// patchDataString patches a single DataKey with the given raw JSON string.
func (d ConfigMapDoc) patchDataString(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	raw string,
) error {
	patch := []byte(fmt.Sprintf(`{"data":{"%s":%q}}`, d.DataKey, raw))
	_, err := cli.ConfigMaps(d.Namespace).Patch(
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
// --------------------------

// Create or update config map, storing data as JSON at DataKey.
func (d ConfigMapDoc) ensureJson(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	data any,
) error {
	b, err := marshalJsonIndented(data)
	if err != nil {
		return err
	}

	cms := cli.ConfigMaps(d.Namespace)
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
// --------------------------

// Patch only DataKey via merge patch.
func (d ConfigMapDoc) patchJson(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	v any,
) error {
	jsonStr, err := jsonString(v)
	if err != nil {
		return err
	}
	return d.patchDataString(ctx, cli, jsonStr)
}

// -------------------------
// readJson
// --------------------------

// readJson reads DataKey; returns nil if the ConfigMap or key is missing.
func (d ConfigMapDoc) readJson(
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
) ([]byte, error) {
	cm, err := lister(d.Namespace).Get(d.Name)
	if apierrors.IsNotFound(err) || cm == nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(cm.Data[d.DataKey]), nil
}

// -------------------------
// mutateJson
// --------------------------

// mutateJson loads -> mutates -> patches an array JSON.
func mutateJson[T any](
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	doc ConfigMapDoc,
	f func(existing []T) ([]T, error),
) error {
	raw, err := doc.readJson(lister)
	if err != nil {
		return err
	}
	var arr []T
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &arr) // best effort
	}
	out, err := f(arr)
	if err != nil {
		return err
	}
	return doc.patchJson(ctx, cli, out)
}

// -------------------------
// mutateRaw
// --------------------------

// mutateRaw loads the JSON string at DataKey, mutates it, and writes the result
// back.
func (d ConfigMapDoc) mutateRaw(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	mutate func(raw []byte) ([]byte, error),
) error {
	raw, err := d.readJson(lister)
	if err != nil || len(raw) == 0 {
		// nil if missing -> no-op
		return err
	}
	newRaw, err := mutate(raw)
	if err != nil || newRaw == nil {
		// nil newRaw -> no-op
		return err
	}
	return d.patchDataString(ctx, cli, string(newRaw))
}
