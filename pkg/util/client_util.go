package util

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
