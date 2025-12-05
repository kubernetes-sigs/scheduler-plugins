// pkg/mypriorityoptimizer/plugin_test.go
// plugin_test.go

package mypriorityoptimizer

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// fakeHandleDeps is a tiny handle that satisfies handleDeps for tests.
type fakeHandleDeps struct {
	cfg     *rest.Config
	factory informers.SharedInformerFactory
}

func (f *fakeHandleDeps) KubeConfig() *rest.Config {
	return f.cfg
}

func (f *fakeHandleDeps) SharedInformerFactory() informers.SharedInformerFactory {
	return f.factory
}

// -----------------------------------------------------------------------------
// Name()
// -----------------------------------------------------------------------------

// TestName verifies that the plugin's Name() method returns the
// global Name constant. This also exercises the trivial method
// receiver path on *SharedState.
func TestName(t *testing.T) {
	pl := &SharedState{}
	got := pl.Name()
	if got != Name {
		t.Fatalf("Name() = %q, want %q", got, Name)
	}
}

// -----------------------------------------------------------------------------
// New (panic on nil handle)
// -----------------------------------------------------------------------------

// TestNewWithNilHandlePanics ensures that if the scheduler ever calls New
// with a nil framework.Handle, we fail loudly rather than silently.
func TestNewWithNilHandlePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected New to panic when given a nil Handle, but it did not panic")
		}
	}()

	_, _ = New(context.Background(), nil, nil) // nil framework.Handle
}

// -----------------------------------------------------------------------------
// newFromHandle – client error path
// -----------------------------------------------------------------------------

func TestNewFromHandle_ClientError(t *testing.T) {
	ctx := context.Background()

	fh := &fakeHandleDeps{
		cfg:     &rest.Config{},
		factory: informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0),
	}

	wantErr := errors.New("boom")
	clientFn := func(*rest.Config) (kubernetes.Interface, error) {
		return nil, wantErr
	}

	// Ensure hooks are not accidentally invoked on error.
	oldReadiness := pluginReadinessStarter
	oldHTTP := httpServerStarter
	defer func() {
		pluginReadinessStarter = oldReadiness
		httpServerStarter = oldHTTP
	}()

	readinessCalled := false
	httpCalled := false
	pluginReadinessStarter = func(pl *SharedState, ctx context.Context, inf ...cache.SharedIndexInformer) {
		readinessCalled = true
	}
	httpServerStarter = func(pl *SharedState, ctx context.Context, addr string) {
		httpCalled = true
	}

	gotPl, err := newFromHandle(ctx, nil, clientFn, fh, nil)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
	if gotPl != nil {
		t.Fatalf("expected plugin nil on client error, got %#v", gotPl)
	}
	if readinessCalled {
		t.Fatalf("pluginReadinessStarter should not be called on client error")
	}
	if httpCalled {
		t.Fatalf("httpServerStarter should not be called on client error")
	}
}

// -----------------------------------------------------------------------------
// newFromHandle – ErrNoSolverEnabled path
// -----------------------------------------------------------------------------

func TestNewFromHandle_NoSolverEnabled(t *testing.T) {
	ctx := context.Background()

	client := fake.NewSimpleClientset()
	fh := &fakeHandleDeps{
		cfg:     &rest.Config{},
		factory: informers.NewSharedInformerFactory(client, 0),
	}

	// Override hooks and solverEnabled.
	oldReadiness := pluginReadinessStarter
	oldHTTP := httpServerStarter
	oldSolver := solverEnabled
	defer func() {
		pluginReadinessStarter = oldReadiness
		httpServerStarter = oldHTTP
		solverEnabled = oldSolver
	}()

	readinessCalled := false
	httpCalled := false
	pluginReadinessStarter = func(pl *SharedState, ctx context.Context, inf ...cache.SharedIndexInformer) {
		readinessCalled = true
	}
	httpServerStarter = func(pl *SharedState, ctx context.Context, addr string) {
		httpCalled = true
	}
	solverEnabled = func(pl *SharedState) bool { return false }

	clientFn := func(*rest.Config) (kubernetes.Interface, error) {
		return client, nil
	}

	gotPl, err := newFromHandle(ctx, nil, clientFn, fh, nil)
	if err != ErrNoSolverEnabled {
		t.Fatalf("expected ErrNoSolverEnabled, got %v", err)
	}
	if gotPl != nil {
		t.Fatalf("expected plugin nil when no solver enabled, got %#v", gotPl)
	}
	if readinessCalled {
		t.Fatalf("pluginReadinessStarter should not be called when no solver is enabled")
	}
	if httpCalled {
		t.Fatalf("httpServerStarter should not be called when no solver is enabled")
	}
}

// -----------------------------------------------------------------------------
// newFromHandle – happy path
// -----------------------------------------------------------------------------

func TestNewFromHandle_Success(t *testing.T) {
	ctx := context.Background()

	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	fh := &fakeHandleDeps{
		cfg:     &rest.Config{},
		factory: factory,
	}

	// Override hooks and solverEnabled.
	oldReadiness := pluginReadinessStarter
	oldHTTP := httpServerStarter
	oldSolver := solverEnabled
	defer func() {
		pluginReadinessStarter = oldReadiness
		httpServerStarter = oldHTTP
		solverEnabled = oldSolver
	}()

	readinessCalled := false
	readinessInformerCount := 0
	pluginReadinessStarter = func(pl *SharedState, ctx context.Context, inf ...cache.SharedIndexInformer) {
		readinessCalled = true
		readinessInformerCount = len(inf)
	}

	httpCalled := false
	httpAddr := ""
	httpServerStarter = func(pl *SharedState, ctx context.Context, addr string) {
		httpCalled = true
		httpAddr = addr
	}

	solverEnabled = func(pl *SharedState) bool { return true }

	clientFn := func(*rest.Config) (kubernetes.Interface, error) {
		return client, nil
	}

	gotPlugin, err := newFromHandle(ctx, nil, clientFn, fh, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	pl, ok := gotPlugin.(*SharedState)
	if !ok {
		t.Fatalf("expected *SharedState, got %T", gotPlugin)
	}

	if pl.Client != client {
		t.Fatalf("expected pl.Client to be fake clientset, got %#v", pl.Client)
	}
	if pl.BlockedWhileActive == nil {
		t.Fatalf("expected BlockedWhileActive to be initialized")
	}

	if !readinessCalled {
		t.Fatalf("expected pluginReadinessStarter to be called on success path")
	}
	// There should be 7 informers: Pods, Nodes, ConfigMaps, ReplicaSets, StatefulSets, DaemonSets, Jobs.
	if readinessInformerCount != 7 {
		t.Fatalf("expected 7 informers passed to pluginReadinessStarter, got %d", readinessInformerCount)
	}

	if !httpCalled {
		t.Fatalf("expected httpServerStarter to be called on success path")
	}
	if httpAddr != HTTPAddr {
		t.Fatalf("expected httpServerStarter addr=%q, got %q", HTTPAddr, httpAddr)
	}
}
