// test_fakes.go
package mypriorityoptimizer

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type fakePodLister struct {
	store     map[string]map[string]*v1.Pod
	err       error
	errPerKey map[string]error
}

func (f *fakePodLister) List(_ labels.Selector) ([]*v1.Pod, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*v1.Pod
	for _, nsMap := range f.store {
		for _, p := range nsMap {
			out = append(out, p)
		}
	}
	return out, nil
}

type fakePodNamespaceLister struct {
	ns        string
	store     map[string]map[string]*v1.Pod
	err       error
	errPerKey map[string]error
}

func (f *fakePodLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return &fakePodNamespaceLister{
		ns:        namespace,
		store:     f.store,
		err:       f.err,
		errPerKey: f.errPerKey,
	}
}

func (f *fakePodNamespaceLister) List(_ labels.Selector) ([]*v1.Pod, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*v1.Pod
	if nsMap, ok := f.store[f.ns]; ok {
		for _, p := range nsMap {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePodNamespaceLister) Get(name string) (*v1.Pod, error) {
	key := f.ns + "/" + name

	// Per-key error overrides everything else.
	if err, ok := f.errPerKey[key]; ok {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}

	nsMap := f.store[f.ns]
	if nsMap == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, name)
	}
	p, ok := nsMap[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, name)
	}
	return p, nil
}

type fakeHandle struct {
	cfg *rest.Config
	framework.Handle
	client  kubernetes.Interface
	factory informers.SharedInformerFactory
}

func (f *fakeHandle) KubeConfig() *rest.Config {
	return f.cfg
}

func (f *fakeHandle) ClientSet() kubernetes.Interface {
	return f.client
}

func (f *fakeHandle) SharedInformerFactory() informers.SharedInformerFactory {
	return f.factory
}

type fakeNodeLister struct {
	nodes []*v1.Node
	err   error
}

func (f *fakeNodeLister) List(selector labels.Selector) ([]*v1.Node, error) {
	return f.nodes, f.err
}

func (f *fakeNodeLister) Get(name string) (*v1.Node, error) {
	for _, n := range f.nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func withNodeLister(nl corev1listers.NodeLister, fn func()) {
	orig := nodesListerFor
	nodesListerFor = func(pl *SharedState) corev1listers.NodeLister { return nl }
	defer func() { nodesListerFor = orig }()
	fn()
}

func withPodLister(plister corev1listers.PodLister, fn func()) {
	orig := podsListerFor
	podsListerFor = func(pl *SharedState) corev1listers.PodLister { return plister }
	defer func() { podsListerFor = orig }()
	fn()
}

func withEvictHook(hook func(pl *SharedState, ctx context.Context, pod *v1.Pod, ev *policyv1.Eviction) error, fn func()) {
	orig := evictPodFor
	evictPodFor = hook
	defer func() { evictPodFor = orig }()
	fn()
}

func withCreateHook(hook func(pl *SharedState, ctx context.Context, pod *v1.Pod) (*v1.Pod, error), fn func()) {
	orig := createPodFor
	createPodFor = hook
	defer func() { createPodFor = orig }()
	fn()
}
