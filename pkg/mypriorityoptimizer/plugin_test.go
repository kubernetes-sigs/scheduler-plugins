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

// -------------------------
// Name
// -------------------------

// TestName ensures the Name method returns the expected plugin name.
func TestName(t *testing.T) {
	pl := &SharedState{}
	got := pl.Name()
	if got != Name {
		t.Fatalf("Name() = %q, want %q", got, Name)
	}
}

// -------------------------
// New
// -------------------------

// TestNew_WithNilHandlePanics ensures that if the scheduler ever calls New
// with a nil framework.Handle, we fail loudly rather than silently.
func TestNew_NilHandlePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected New to panic when given a nil Handle, but it did not panic")
		}
	}()

	_, _ = New(context.Background(), nil, nil) // nil framework.Handle
}

// TestNew_Handle_ClientError ensures that if there is an error creating
// the Kubernetes client from the provided rest.Config, the error is
// propagated and no hooks are invoked.
func TestNew_Handle_ClientError(t *testing.T) {
	ctx := context.Background()

	fh := &fakeHandle{
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

// TestNew_Handle_NoSolverEnabled ensures that if no solver is enabled,
// New returns ErrNoSolverEnabled and does not invoke any hooks.
func TestNew_Handle_NoSolverEnabled(t *testing.T) {
	ctx := context.Background()

	client := fake.NewSimpleClientset()
	fh := &fakeHandle{
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

// TestNew_Handle_Success ensures that when all conditions are met,
// New returns a properly initialized plugin and invokes the expected hooks.
func TestNew_Handle_Success(t *testing.T) {
	ctx := context.Background()

	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	fh := &fakeHandle{
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
