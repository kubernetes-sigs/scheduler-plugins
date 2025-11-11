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

// Create or update config map, storing data as JSON at DataKey.
func (d ConfigMapDoc) ensureJson(ctx context.Context, cli corev1client.CoreV1Interface, data any) error {
	b, err := json.MarshalIndent(data, "", "  ")
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

// Patch only DataKey via merge patch.
func (d ConfigMapDoc) patchJson(ctx context.Context, cli corev1client.CoreV1Interface, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"data":{"%s":%q}}`, d.DataKey, string(b)))
	_, err = cli.ConfigMaps(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Read DataKey; nil if config map or key missing.
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

// Load -> mutate -> patch an array JSON.
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

// MutateRaw loads the JSON string at DataKey, 'mutate' it, and writes the result back.
func (d ConfigMapDoc) mutateRaw(
	ctx context.Context,
	cli corev1client.CoreV1Interface,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	mutate func(raw []byte) ([]byte, error),
) error {
	raw, err := d.readJson(lister)
	if err != nil || len(raw) == 0 {
		return err // nil if missing -> no-op
	}
	newRaw, err := mutate(raw)
	if err != nil || newRaw == nil {
		return err // nil -> no-op
	}
	// Store as string (ConfigMap data values are strings).
	patch := []byte(fmt.Sprintf(`{"data":{"%s":%q}}`, d.DataKey, string(newRaw)))
	_, err = cli.ConfigMaps(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// List config maps by label newest-first.
func listConfigMaps(
	_ context.Context,
	lister func(ns string) corev1listers.ConfigMapNamespaceLister,
	namespace, labelKey string,
) ([]apiv1.ConfigMap, error) {
	sel := labels.SelectorFromSet(labels.Set{labelKey: "true"})
	items, err := lister(namespace).List(sel)
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.ConfigMap, len(items))
	for i := range items {
		out[i] = *items[i].DeepCopy()
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreationTimestamp.Time.After(out[j].CreationTimestamp.Time)
	})
	return out, nil
}

// Keep first K newest config maps for label, delete the rest.
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
