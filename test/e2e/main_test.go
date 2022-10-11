//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/klog/v2"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

var (
	kindClusterName           = "scheduler-plugins-e2e-test"
	registry                  = os.Getenv("LOCAL_REGISTRY")
	controllerImageName       = os.Getenv("LOCAL_CONTROLLER_IMAGE")
	KubeSchedImageName        = os.Getenv("LOCAL_IMAGE")
	providerResourceDirectory = "manifests/install/charts/as-a-second-scheduler/"
	testenv                   env.Environment
)

func TestMain(m *testing.M) {
	testenv = env.NewWithConfig(envconf.New())
	// Create KinD Cluster
	namespace := "scheduler-plugins"
	testenv.Setup(
		envfuncs.CreateKindClusterWithConfig(kindClusterName, "kindest/node:v1.24.3", "kind-config.yaml"),
		envfuncs.LoadDockerImageToCluster(kindClusterName, fmt.Sprintf("%s/%s", registry, controllerImageName)),
		envfuncs.LoadDockerImageToCluster(kindClusterName, fmt.Sprintf("%s/%s", registry, KubeSchedImageName)),
		deploySchedulerPluginsChart(namespace),
	).Finish( // Cleanup KinD Cluster
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyKindCluster(kindClusterName),
	)
	os.Exit(testenv.Run(m))
}

func deploySchedulerPluginsChart(namespace string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		wd, err := os.Getwd()
		if err != nil {
			return ctx, err
		}
		providerResourceAbsolutePath, err := filepath.Abs(filepath.Join(wd, "/../../", providerResourceDirectory))
		if err != nil {
			return ctx, err
		}

		manager := helm.New(cfg.KubeconfigFile())

		args := fmt.Sprintf("--set scheduler.image=%s --set controller.image=%s",
			fmt.Sprintf("%s/%s", registry, KubeSchedImageName),
			fmt.Sprintf("%s/%s", registry, controllerImageName))

		if err := manager.RunInstall(helm.WithName("scheduler-plugins"),
			helm.WithChart(providerResourceAbsolutePath),
			helm.WithArgs(args),
			helm.WithWait(), helm.WithTimeout("1m")); err != nil {
			klog.ErrorS(err, "failed to invoke helm install operation due to an error")
		}

		deployment := &appsv1.Deployment{
			ObjectMeta: v1.ObjectMeta{
				Name:      "scheduler-plugins-scheduler",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{},
		}

		if err := wait.For(conditions.New(cfg.Client().Resources()).ResourceScaled(deployment, func(object k8s.Object) int32 {
			return object.(*appsv1.Deployment).Status.ReadyReplicas
		}, 1), wait.WithTimeout(time.Minute*1)); err != nil {

			klog.ErrorS(err, " Failed to deploy scheduler plguins")
			return ctx, err
		}

		return ctx, nil
	}
}
