/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

type ElasticQuotaReconciler struct {
	recorder record.EventRecorder

	client.Client
	Scheme  *runtime.Scheme
	Workers int
}

// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=elasticquota,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=elasticquota/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=elasticquota/finalizers,verbs=update
func (r *ElasticQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("reconciling")
	eqList := &schedv1alpha1.ElasticQuotaList{}
	if err := r.List(ctx, eqList, client.InNamespace(req.Namespace)); err != nil {
		if apierrs.IsNotFound(err) {
			log.V(5).Info("no elasticquota found")
			return ctrl.Result{}, nil
		}
		log.V(3).Error(err, "Unable to retrieve elasticquota")
		return ctrl.Result{}, err
	}

	// TODO: When elastic quota supports multiple instances in a namespace, modify this
	if len(eqList.Items) == 0 {
		log.V(5).Info("no elasticquota found")
		return ctrl.Result{}, nil
	}

	eq := &eqList.Items[0]
	used, err := r.computeElasticQuotaUsed(ctx, req.Namespace, eq)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ignore this loop if the usage value has not changed
	if apiequality.Semantic.DeepEqual(used, eq.Status.Used) {
		return ctrl.Result{}, nil
	}

	// create a usage object that is based on the elastic quota version that will handle updates
	// by default, we set used to the current status
	newEQ := eq.DeepCopy()
	newEQ.Status.Used = used
	if err = r.patchElasticQuota(ctx, eq, newEQ); err != nil {
		return ctrl.Result{}, err
	}
	r.recorder.Event(eq, v1.EventTypeNormal, "Synced", fmt.Sprintf("Elastic Quota %s synced successfully", req.NamespacedName))
	return ctrl.Result{}, nil
}

func (r *ElasticQuotaReconciler) patchElasticQuota(ctx context.Context, old, new *schedv1alpha1.ElasticQuota) error {
	patch := client.MergeFrom(old)
	return r.Status().Patch(ctx, new, patch)
}

func (r *ElasticQuotaReconciler) computeElasticQuotaUsed(ctx context.Context, namespace string, eq *schedv1alpha1.ElasticQuota) (v1.ResourceList, error) {
	used := newZeroUsed(eq)
	podList := &v1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for _, p := range podList.Items {
		if p.Status.Phase == v1.PodRunning {
			used = quota.Add(used, computePodResourceRequest(&p))
		}
	}
	return used, nil
}

// computePodResourceRequest returns a v1.ResourceList that covers the largest
// width in each resource dimension. Because init-containers run sequentially, we collect
// the max in each dimension iteratively. In contrast, we sum the resource vectors for
// regular containers since they run simultaneously.
//
// If Pod Overhead is specified and the feature gate is set, the resources defined for Overhead
// are added to the calculated Resource request sum
//
// Example:
//
// Pod:
//
//	InitContainers
//	  IC1:
//	    CPU: 2
//	    Memory: 1G
//	  IC2:
//	    CPU: 2
//	    Memory: 3G
//	Containers
//	  C1:
//	    CPU: 2
//	    Memory: 1G
//	  C2:
//	    CPU: 1
//	    Memory: 1G
//
// Result: CPU: 3, Memory: 3G
func computePodResourceRequest(pod *v1.Pod) v1.ResourceList {
	result := v1.ResourceList{}
	for _, container := range pod.Spec.Containers {
		result = quota.Add(result, container.Resources.Requests)
	}
	initRes := v1.ResourceList{}
	// take max_resource for init_containers
	for _, container := range pod.Spec.InitContainers {
		initRes = quota.Max(initRes, container.Resources.Requests)
	}
	// If Overhead is being utilized, add to the total requests for the pod
	if pod.Spec.Overhead != nil {
		quota.Add(result, pod.Spec.Overhead)
	}
	// take max_resource for init_containers and containers
	return quota.Max(result, initRes)
}

// newZeroUsed will return the zero value of the union of min and max
func newZeroUsed(eq *schedv1alpha1.ElasticQuota) v1.ResourceList {
	minResources := quota.ResourceNames(eq.Spec.Min)
	maxResources := quota.ResourceNames(eq.Spec.Max)
	res := v1.ResourceList{}
	for _, v := range minResources {
		res[v] = *resource.NewQuantity(0, resource.DecimalSI)
	}
	for _, v := range maxResources {
		res[v] = *resource.NewQuantity(0, resource.DecimalSI)
	}
	return res
}

func (r *ElasticQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("ElasticQuotaController")
	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1.Pod{}, &handler.EnqueueRequestForObject{}).
		For(&schedv1alpha1.ElasticQuota{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Workers}).
		Complete(r)
}
