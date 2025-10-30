package util

import (
	"context"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LabelIndexerName = "label-indexer"
)

// NewClientWithCachedReader returns a controller runtime Client with cache-baked client.
func NewClientWithCachedReader(ctx context.Context, config *rest.Config, scheme *runtime.Scheme) (client.Client, cache.Cache, error) {
	ccache, err := cache.New(config, cache.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, nil, err
	}
	go ccache.Start(ctx)
	if !ccache.WaitForCacheSync(ctx) {
		return nil, nil, fmt.Errorf("failed to sync cache")
	}
	c, err := client.New(config, client.Options{
		Scheme: scheme,
		Cache: &client.CacheOptions{
			Reader: ccache,
		},
	})
	return c, ccache, err
}

// NewIndexByLabelAndNamespace returns an indexer function for indexing pods by label and namespace.
func NewIndexByLabelAndNamespace(label string) func(obj interface{}) ([]string, error) {
	return func(obj interface{}) ([]string, error) {
		labels := obj.(metav1.Object).GetLabels()
		if labels == nil {
			return nil, nil
		}
		if labels[label] == "" {
			return nil, nil
		}
		namespace := obj.(metav1.Object).GetNamespace()

		return []string{namespace + "/" + labels[label]}, nil
	}
}
