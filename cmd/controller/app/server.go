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

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"sigs.k8s.io/scheduler-plugins/pkg/controller"
	schedclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	schedformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
)

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
	pgInformer := schedInformerFactory.Scheduling().V1alpha1().PodGroups()
	eqInformer := schedInformerFactory.Scheduling().V1alpha1().ElasticQuotas()

	coreInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := coreInformerFactory.Core().V1().Pods()
	pgCtrl := controller.NewPodGroupController(kubeClient, pgInformer, podInformer, schedClient)
	eqCtrl := controller.NewElasticQuotaController(kubeClient, eqInformer, podInformer, schedClient)

	run := func(ctx context.Context) {
		go pgCtrl.Run(s.Workers, ctx.Done())
		go eqCtrl.Run(s.Workers, ctx.Done())
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
