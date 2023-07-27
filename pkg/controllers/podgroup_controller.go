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
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	schedv1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/util"
)

// PodGroupReconciler reconciles a PodGroup object
type PodGroupReconciler struct {
	log      logr.Logger
	recorder record.EventRecorder

	client.Client
	Scheme  *runtime.Scheme
	Workers int
}

// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=podgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=podgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=scheduling.x-k8s.io,resources=podgroups/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodGroup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *PodGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("reconciling")
	pg := &schedv1alpha1.PodGroup{}
	if err := r.Get(ctx, req.NamespacedName, pg); err != nil {
		if apierrs.IsNotFound(err) {
			log.V(5).Info("Pod group has been deleted")
			return ctrl.Result{}, nil
		}
		log.V(3).Error(err, "Unable to retrieve pod group")
		return ctrl.Result{}, err
	}

	if pg.Status.Phase == schedv1alpha1.PodGroupFinished ||
		pg.Status.Phase == schedv1alpha1.PodGroupFailed {
		return ctrl.Result{}, nil
	}
	// If startScheduleTime - createTime > 2days,
	// do not reconcile again because pod may have been GCed
	if (pg.Status.Phase == schedv1alpha1.PodGroupScheduling || pg.Status.Phase == schedv1alpha1.PodGroupPending) && pg.Status.Running == 0 &&
		pg.Status.ScheduleStartTime.Sub(pg.CreationTimestamp.Time) > 48*time.Hour {
		r.recorder.Event(pg, v1.EventTypeWarning,
			"Timeout", "schedule time longer than 48 hours")
		return ctrl.Result{}, nil
	}

	podList := &v1.PodList{}
	if err := r.List(ctx, podList,
		client.MatchingLabelsSelector{
			Selector: labels.Set(map[string]string{
				schedv1alpha1.PodGroupLabel: pg.Name}).AsSelector(),
		}); err != nil {
		log.Error(err, "List pods for group failed")
		return ctrl.Result{}, err
	}
	pods := podList.Items

	pgCopy := pg.DeepCopy()
	switch pgCopy.Status.Phase {
	case "":
		pgCopy.Status.Phase = schedv1alpha1.PodGroupPending
	case schedv1alpha1.PodGroupPending:
		if len(pods) >= int(pg.Spec.MinMember) {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupScheduling
			fillOccupiedObj(pgCopy, &pods[0])
		}
	default:
		pgCopy.Status.Running, pgCopy.Status.Succeeded, pgCopy.Status.Failed = getCurrentPodStats(pods)
		if len(pods) < int(pg.Spec.MinMember) {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupPending
			break
		}

		if pgCopy.Status.Succeeded+pgCopy.Status.Running < pg.Spec.MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupScheduling
		}

		if pgCopy.Status.Succeeded+pgCopy.Status.Running >= pg.Spec.MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupRunning
		}
		// Final state of pod group
		if pgCopy.Status.Failed != 0 &&
			pgCopy.Status.Failed+pgCopy.Status.Running+pgCopy.Status.Succeeded >= pg.Spec.MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupFailed
		}
		if pgCopy.Status.Succeeded >= pg.Spec.MinMember {
			pgCopy.Status.Phase = schedv1alpha1.PodGroupFinished
		}
	}

	return r.patchPodGroup(ctx, pg, pgCopy)
}

func (r *PodGroupReconciler) patchPodGroup(ctx context.Context, old, new *schedv1alpha1.PodGroup) (ctrl.Result, error) {
	patch := client.MergeFrom(old)
	if err := r.Status().Patch(ctx, new, patch); err != nil {
		return ctrl.Result{}, err
	}
	err := r.Patch(ctx, new, patch)
	return ctrl.Result{}, err
}

func getCurrentPodStats(pods []v1.Pod) (int32, int32, int32) {
	if len(pods) == 0 {
		return 0, 0, 0
	}

	var (
		running   int32 = 0
		succeeded int32 = 0
		failed    int32 = 0
	)
	for _, pod := range pods {
		switch pod.Status.Phase {
		case v1.PodRunning:
			running++
		case v1.PodSucceeded:
			succeeded++
		case v1.PodFailed:
			failed++
		}
	}
	return running, succeeded, failed
}

func fillOccupiedObj(pg *schedv1alpha1.PodGroup, pod *v1.Pod) {
	if len(pod.OwnerReferences) == 0 {
		return
	}

	var refs []string
	for _, ownerRef := range pod.OwnerReferences {
		refs = append(refs, fmt.Sprintf("%s/%s", pod.Namespace, ownerRef.Name))
	}
	if len(refs) != 0 {
		sort.Strings(refs)
		pg.Status.OccupiedBy = strings.Join(refs, ",")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("PodGroupController")
	r.log = mgr.GetLogger()

	return ctrl.NewControllerManagedBy(mgr).
		Watches(&v1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToPodGroup)).
		For(&schedv1alpha1.PodGroup{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Workers}).
		Complete(r)
}

func (r *PodGroupReconciler) podToPodGroup(ctx context.Context, obj client.Object) []ctrl.Request {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil
	}
	pgName := util.GetPodGroupLabel(pod)
	if len(pgName) == 0 {
		return nil
	}

	r.log.V(5).Info("Add pod group when pod gets added", "podGroup", pgName, "pod", pod.Name, "namespace", pod.Namespace)

	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      pgName,
		}}}
}
