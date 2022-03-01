//go:build e2e
// +build e2e

// https://raw.githubusercontent.com/Azure/secrets-store-csi-driver-provider-azure/master/test/e2e/framework/exec/kubectl.go

package e2e

import (
	"fmt"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
)

// KubectlApply executes "kubectl apply" given a list of arguments.
func KubectlApply(kubeconfigPath, namespace string, args []string) error {
	args = append([]string{
		"apply",
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		fmt.Sprintf("--namespace=%s", namespace),
	}, args...)

	_, err := kubectl(args)
	return err
}

// KubectlDelete executes "kubectl delete" given a list of arguments.
func KubectlDelete(kubeconfigPath, namespace string, args []string) error {
	args = append([]string{
		"delete",
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		fmt.Sprintf("--namespace=%s", namespace),
	}, args...)

	_, err := kubectl(args)
	return err
}

// KubectlExec executes "kubectl exec" given a list of arguments.
func KubectlExec(kubeconfigPath, podName, namespace string, args []string) (string, error) {
	args = append([]string{
		"exec",
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		fmt.Sprintf("--namespace=%s", namespace),
		"--request-timeout=5s",
		podName,
		"--",
	}, args...)

	return kubectl(args)
}

// KubectlLogs executes "kubectl logs" given a list of arguments.
func KubectlLogs(kubeconfigPath, podName, containerName, namespace string) (string, error) {
	args := []string{
		"logs",
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		fmt.Sprintf("--namespace=%s", namespace),
		podName,
	}

	if containerName != "" {
		args = append(args, fmt.Sprintf("-c=%s", containerName))
	}

	return kubectl(args)
}

// KubectlDescribe executes "kubectl describe" given a list of arguments.
func KubectlDescribe(kubeconfigPath, podName, namespace string) (string, error) {
	args := []string{
		"describe",
		"pod",
		podName,
		fmt.Sprintf("--kubeconfig=%s", kubeconfigPath),
		fmt.Sprintf("--namespace=%s", namespace),
	}
	return kubectl(args)
}

func kubectl(args []string) (string, error) {
	klog.Infof("kubectl %s", strings.Join(args, " "))

	cmd := exec.Command("kubectl", args...)
	stdoutStderr, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(stdoutStderr)), err
}
