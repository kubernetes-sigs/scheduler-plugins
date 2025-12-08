// plugin_readiness_test.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

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

		// Give pluginReadiness a moment to enter WaitForCacheSync, then cancel.
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
			// No informers; should short-circuit on ctx cancellation or waitForUsableNode.
			pl.pluginReadiness(ctx)
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
				// No informers; cacheReady will trivially succeed, but warmup should
				// observe ctx cancellation and return.
				pl.pluginReadiness(ctx)
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
				// Use PerPod@PreEnqueue so we are in the most restrictive mode;
				// readiness itself should still complete once a usable node is seen.
				withMode(ModePerPod, true, func() {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					withReadinessHooks(
						func(_ *SharedState) ([]*v1.Node, error) {
							// Immediately report one node, so readiness does not spin.
							return []*v1.Node{new(v1.Node)}, nil
						},
						func(*v1.Node) bool { return true },
						func() {
							done := make(chan struct{})
							go func() {
								pl.pluginReadiness(ctx)
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
			errs := 0
			withReadinessHooks(
				func(_ *SharedState) ([]*v1.Node, error) {
					calls++
					if calls == 1 {
						errs++
						return nil, fmt.Errorf("boom") // first call: error
					}
					// After the first error, simulate nodes present but none usable,
					// and cancel the context. There might be 1 or more of these calls
					// before ctx.Done() wins the next select.
					cancel()
					return []*v1.Node{new(v1.Node)}, nil
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
						t.Fatalf("expected exactly 2 getNodes calls (error + unusable), got %d", calls)
					}
					if errs != 1 {
						t.Fatalf("expected exactly 1 getNodes error, got %d", errs)
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
