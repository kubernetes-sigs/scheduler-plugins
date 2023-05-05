/*
Copyright 2020 The Kubernetes Authors.

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

package app

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2/klogr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	schedulingv1a1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(schedulingv1a1.AddToScheme(scheme))
}

func Run(s *ServerRunOptions) error {
	config := ctrl.GetConfigOrDie()
	config.QPS = float32(s.ApiServerQPS)
	config.Burst = s.ApiServerBurst

	// Controller Runtime Controllers
	ctrl.SetLogger(klogr.New())
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		MetricsBindAddress:      s.MetricsAddr,
		Port:                    9443,
		HealthProbeBindAddress:  s.ProbeAddr,
		LeaderElection:          s.EnableLeaderElection,
		LeaderElectionID:        "sched-plugins-controllers",
		LeaderElectionNamespace: "kube-system",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}

	if err = (&controllers.PodGroupReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Workers: s.Workers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodGroup")
		return err
	}

	if err = (&controllers.ElasticQuotaReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Workers: s.Workers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ElasticQuota")
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		return err
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}
	return nil
}
