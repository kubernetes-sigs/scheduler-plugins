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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	coreinformer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"sigs.k8s.io/scheduler-plugins/pkg/controller"
	pgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	"sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/scheme"
	pgformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
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
		klog.Fatal(err)
	}
	config.QPS = float32(s.ApiServerQPS)
	config.Burst = s.ApiServerBurst
	stopCh := server.SetupSignalHandler()

	pgClient := pgclientset.NewForConfigOrDie(config)
	kubeClient := kubernetes.NewForConfigOrDie(config)

	pgInformerFactory := pgformers.NewSharedInformerFactory(pgClient, 0)
	pgInformer := pgInformerFactory.Scheduling().V1alpha1().PodGroups()

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 0, informers.WithTweakListOptions(func(opt *metav1.ListOptions) {
	}))
	podInformer := informerFactory.Core().V1().Pods()
	pgCtrl := controller.NewPodGroupController(kubeClient, pgInformer, podInformer, pgClient)
	eqCtrl := initEQController(kubeClient, pgInformerFactory, pgClient, podInformer)

	run := func(ctx context.Context) {
		go pgCtrl.Run(s.Workers, ctx.Done())
		eqCtrl.Run(s.Workers, ctx.Done())
	}
	pgInformerFactory.Start(stopCh)
	informerFactory.Start(stopCh)
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
			klog.Fatalf("error creating lock: %v", err)
		}

		leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
			Lock: rl,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: run,
				OnStoppedLeading: func() {
					klog.Fatalf("leaderelection lost")
				},
			},
			Name: "scheduler-plugins controller",
		})
	}

	<-stopCh
	return nil
}

func initEQController(kubeClient *kubernetes.Clientset, pgInformerFactory pgformers.SharedInformerFactory, pgClient *pgclientset.Clientset, podInformer coreinformer.PodInformer) *controller.ElasticQuotaController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events(v1.NamespaceAll)})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "ElasticQuota-Controller"})

	eqInformer := pgInformerFactory.Scheduling().V1alpha1().ElasticQuotas()
	eqCtrl := controller.NewElasticQuotaController(kubeClient, pgClient, eqInformer, podInformer, recorder)
	return eqCtrl
}
