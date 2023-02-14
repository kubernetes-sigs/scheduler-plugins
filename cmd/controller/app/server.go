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
	"context"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	schedulingv1a1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/controller"
	"sigs.k8s.io/scheduler-plugins/pkg/controllers"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(schedulingv1a1.AddToScheme(scheme))
}

func newConfig(kubeconfig, master string, inCluster bool) (*restclient.Config, error) {
	var (
		config *rest.Config
		err    error
	)
	if inCluster {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags(master, kubeconfig)
	}
	if err != nil {
		return nil, err
	}
	return config, nil
}

func Run(s *ServerRunOptions) error {
	ctx := context.Background()
	config, err := newConfig(s.KubeConfig, s.MasterUrl, s.InCluster)
	if err != nil {
		klog.ErrorS(err, "Failed to parse config")
		os.Exit(1)
	}
	config.QPS = float32(s.ApiServerQPS)
	config.Burst = s.ApiServerBurst
	stopCh := server.SetupSignalHandler()
	schedClient := schedclientset.NewForConfigOrDie(config)
	kubeClient := kubernetes.NewForConfigOrDie(config)

	schedInformerFactory := schedformers.NewSharedInformerFactory(schedClient, 0)
	eqInformer := schedInformerFactory.Scheduling().V1alpha1().ElasticQuotas()

	coreInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := coreInformerFactory.Core().V1().Pods()
	eqCtrl := controller.NewElasticQuotaController(kubeClient, eqInformer, podInformer, schedClient)

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
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PodGroup")
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

	run := func(ctx context.Context) {
		go eqCtrl.Run(s.Workers, ctx.Done())
		setupLog.Info("starting manager")
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			setupLog.Error(err, "unable to start manager")
			panic(err)
		}

		select {}
	}
	schedInformerFactory.Start(stopCh)
	coreInformerFactory.Start(stopCh)
	if !s.EnableLeaderElection {
		run(ctx)
	} else {
		id, err := os.Hostname()
		if err != nil {
			return err
		}
		// add a uniquifier so that two processes on the same host don't accidentally both become active
		id = id + "_" + string(uuid.NewUUID())

		rl, err := resourcelock.New("endpoints",
			"kube-system",
			"sched-plugins-controller",
			kubeClient.CoreV1(),
			kubeClient.CoordinationV1(),
			resourcelock.ResourceLockConfig{
				Identity: id,
			})
		if err != nil {
			klog.ErrorS(err, "Resource lock creation failed")
			os.Exit(1)
		}

		leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
			Lock: rl,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: run,
				OnStoppedLeading: func() {
					klog.ErrorS(err, "Leaderelection lost")
					os.Exit(1)
				},
			},
			Name: "scheduler-plugins controller",
		})
	}

	<-stopCh
	return nil
}
