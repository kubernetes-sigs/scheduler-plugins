// plugin_readiness_test.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

// fakeSharedIndexInformer is a minimal SharedIndexInformer stub that only
// implements HasSynced in a meaningful way. All other methods are no-ops.
type fakeSharedIndexInformer struct {
	synced bool
}

func (f *fakeSharedIndexInformer) AddEventHandler(
	handler cache.ResourceEventHandler,
) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (f *fakeSharedIndexInformer) AddEventHandlerWithResyncPeriod(
	handler cache.ResourceEventHandler,
	resyncPeriod time.Duration,
) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (f *fakeSharedIndexInformer) RemoveEventHandler(
	reg cache.ResourceEventHandlerRegistration,
) error {
	return nil
}

func (f *fakeSharedIndexInformer) GetStore() cache.Store           { return nil }
func (f *fakeSharedIndexInformer) GetController() cache.Controller { return nil }
func (f *fakeSharedIndexInformer) Run(stopCh <-chan struct{})      {}

func (f *fakeSharedIndexInformer) HasSynced() bool                 { return f.synced }
func (f *fakeSharedIndexInformer) LastSyncResourceVersion() string { return "" }

func (f *fakeSharedIndexInformer) AddIndexers(indexers cache.Indexers) error { return nil }
func (f *fakeSharedIndexInformer) GetIndexer() cache.Indexer                 { return nil }

func (f *fakeSharedIndexInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}

func (f *fakeSharedIndexInformer) SetTransform(handler cache.TransformFunc) error {
	return nil
}

func (f *fakeSharedIndexInformer) IsStopped() bool { return false }

// Override helpers – these now tweak the overrideable vars, not the consts.

func withCacheWarmupDelay(d time.Duration, fn func()) {
	old := cacheWarmupDelay
	cacheWarmupDelay = d
	defer func() { cacheWarmupDelay = old }()
	fn()
}

func withReadinessInterval(d time.Duration, fn func()) {
	old := readinessUsableNodeInterval
	readinessUsableNodeInterval = d
	defer func() { readinessUsableNodeInterval = old }()
	fn()
}

func withReadinessHooks(
	getNodes func(*SharedState) ([]*v1.Node, error),
	isUsable func(*v1.Node) bool,
	fn func(),
) {
	oldGet := getNodesForReadiness
	oldUsable := isNodeUsableForReadiness

	getNodesForReadiness = getNodes
	isNodeUsableForReadiness = isUsable

	defer func() {
		getNodesForReadiness = oldGet
		isNodeUsableForReadiness = oldUsable
	}()

	// run the body passed in from the test
	fn()
}

// -----------------------------------------------------------------------------
// pluginReadiness
// -----------------------------------------------------------------------------

func TestPluginReadiness(t *testing.T) {
	t.Run("cache-sync-canceled", func(t *testing.T) {
		pl := &SharedState{
			BlockedWhileActive: newPodSet("test"),
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		inf := &fakeSharedIndexInformer{synced: false}

		done := make(chan struct{})
		go func() {
			pl.pluginReadiness(ctx, inf)
			close(done)
		}()

		time.Sleep(10 * time.Millisecond)
		cancel()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatalf("pluginReadiness did not return in time when cache sync is canceled")
		}

		if got := pl.PluginReady.Load(); got {
			t.Fatalf("expected PluginReady=false when cache sync is canceled, got true")
		}
	})

	t.Run("ctx-canceled-before-usable-node", func(t *testing.T) {
		pl := &SharedState{
			BlockedWhileActive: newPodSet("test"),
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		go func() {
			pl.pluginReadiness(ctx /* no informers */)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatalf("pluginReadiness did not return in time when ctx is already canceled")
		}

		if got := pl.PluginReady.Load(); got {
			t.Fatalf("expected PluginReady=false when ctx is canceled before usable node, got true")
		}
	})

	t.Run("warmup-canceled", func(t *testing.T) {
		pl := &SharedState{
			BlockedWhileActive: newPodSet("test"),
		}

		withCacheWarmupDelay(50*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			done := make(chan struct{})
			go func() {
				pl.pluginReadiness(ctx /* no informers */)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(1 * time.Second):
				t.Fatalf("pluginReadiness did not return in time when warmup is canceled")
			}

			if got := pl.PluginReady.Load(); got {
				t.Fatalf("expected PluginReady=false when warmup is canceled, got true")
			}
		})
	})

	t.Run("success-path", func(t *testing.T) {
		pl := &SharedState{
			BlockedWhileActive: newPodSet("test"),
		}

		withReadinessInterval(1*time.Millisecond, func() {
			withCacheWarmupDelay(0, func() {
				// Use PerPod@PreEnqueue so pluginReadiness skips activatePods.
				withGlobals(ModePerPod, StagePreEnqueue, true, func() {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					withReadinessHooks(
						func(_ *SharedState) ([]*v1.Node, error) {
							return []*v1.Node{new(v1.Node)}, nil
						},
						func(*v1.Node) bool { return true },
						func() {
							done := make(chan struct{})
							go func() {
								pl.pluginReadiness(ctx /* no informers */)
								close(done)
							}()

							select {
							case <-done:
							case <-time.After(3 * time.Second):
								t.Fatalf("pluginReadiness did not complete in time on success path")
							}

							if !pl.PluginReady.Load() {
								t.Fatalf("expected PluginReady=true on success path")
							}
						},
					)
				})
			})
		})
	})
}

// -----------------------------------------------------------------------------
// isCacheReady
// -----------------------------------------------------------------------------

func TestIsCacheReady(t *testing.T) {
	t.Run("no-informers-returns-true", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if !isCacheReady(ctx) {
			t.Fatalf("expected isCacheReady to return true when no informers are provided")
		}
	})

	t.Run("synced-informer-returns-true", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		inf := &fakeSharedIndexInformer{synced: true}
		if !isCacheReady(ctx, inf) {
			t.Fatalf("expected isCacheReady to return true when informer is synced")
		}
	})

	t.Run("ctx-canceled-returns-false", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		inf := &fakeSharedIndexInformer{synced: false}
		cancel()

		if isCacheReady(ctx, inf) {
			t.Fatalf("expected isCacheReady to return false when context is canceled")
		}
	})

	t.Run("nil-informer-ignored", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if !isCacheReady(ctx, nil) {
			t.Fatalf("expected isCacheReady to return true when only nil informers are passed")
		}
	})
}

// -----------------------------------------------------------------------------
// waitForUsableNode
// -----------------------------------------------------------------------------

func TestWaitForUsableNode(t *testing.T) {
	t.Run("ctx-canceled-before-first-tick", func(t *testing.T) {
		pl := &SharedState{}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if got := pl.waitForUsableNode(ctx); got {
			t.Fatalf("expected waitForUsableNode to return false when ctx is canceled before first tick")
		}
	})

	t.Run("error-and-no-usable", func(t *testing.T) {
		pl := &SharedState{}

		withReadinessInterval(1*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			calls := 0

			withReadinessHooks(
				func(_ *SharedState) ([]*v1.Node, error) {
					calls++
					switch calls {
					case 1:
						return nil, fmt.Errorf("boom") // error path
					case 2:
						defer cancel()
						return []*v1.Node{new(v1.Node)}, nil // no usable nodes
					default:
						return nil, fmt.Errorf("unexpected extra getNodes call")
					}
				},
				func(*v1.Node) bool {
					return false // treat all nodes as unusable
				},
				func() {
					got := pl.waitForUsableNode(ctx)
					if got {
						t.Fatalf("expected waitForUsableNode to return false when only unusable nodes and errors")
					}
					if calls != 2 {
						t.Fatalf("expected 2 getNodes calls, got %d", calls)
					}
				},
			)
		})
	})

	t.Run("usable-node-found", func(t *testing.T) {
		pl := &SharedState{}

		withReadinessInterval(1*time.Millisecond, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			calls := 0

			withReadinessHooks(
				func(_ *SharedState) ([]*v1.Node, error) {
					calls++
					return []*v1.Node{new(v1.Node)}, nil
				},
				func(*v1.Node) bool { return true }, // always usable
				func() {
					got := pl.waitForUsableNode(ctx)
					if !got {
						t.Fatalf("expected waitForUsableNode to return true when a usable node is present")
					}
					if calls == 0 {
						t.Fatalf("expected at least one getNodes call, got %d", calls)
					}
				},
			)
		})
	})
}
