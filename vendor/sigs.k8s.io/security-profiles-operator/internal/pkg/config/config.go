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

package config

import (
	"errors"
	"os"
	"path/filepath"

	"sigs.k8s.io/release-utils/env"
)

const (
	// OperatorName is the name when referring to the operator.
	OperatorName = "security-profiles-operator"

	// SPOdName is the name of the default SPOd config instance.
	SPOdName = "spod"

	// Service Account for the security-profiles-operator daemon.
	SPOdServiceAccount = SPOdName

	// SPOdNameEnvKey allows one to query the name of the SPOd instance
	// from within the daemon.
	SPOdNameEnvKey = "SPOD_NAME"

	// OperatorRoot is the root directory of the operator.
	OperatorRoot = "/var/lib/security-profiles-operator"

	// UserRootless is the user which runs the operator.
	UserRootless = 65535

	// KubeletSeccompRootPath specifies the path where all kubelet seccomp
	// profiles are stored.
	KubeletSeccompRootPath = "/var/lib/kubelet/seccomp"

	// ProfilesRootPath specifies the path where the operator stores seccomp
	// profiles.
	ProfilesRootPath = KubeletSeccompRootPath + "/operator"

	// NodeNameEnvKey is the default environment variable key for retrieving
	// the name of the current node.
	NodeNameEnvKey = "NODE_NAME"

	// OperatorNamespaceEnvKey is the default environment variable key for retrieving
	// the operator's namespace.
	OperatorNamespaceEnvKey = "OPERATOR_NAMESPACE"

	// RestrictNamespaceEnvKey is the environment variable key for restricting
	// the operator to work on only a single Kubernetes namespace.
	RestrictNamespaceEnvKey = "RESTRICT_TO_NAMESPACE"

	// VerbosityEnvKey is the environment variable key for the logging verbosity.
	VerbosityEnvKey = "SPO_VERBOSITY"

	// ProfilingEnvKey is the environment variable key for enabling profiling
	// support.
	ProfilingEnvKey = "SPO_PROFILING"

	// ProfilingPortEnvKey is the environment variable key for choosing the
	// profiling port.
	ProfilingPortEnvKey = "SPO_PROFILING_PORT"

	// DefaultProfilingPort is the start port where the profiling endpoint runs.
	DefaultProfilingPort = 6060

	// SelinuxEnabledByDefaultEnvKey is the environment variable key for enabling
	// SELinux by default if the default SecurityProfilesOperatorDaemon instance
	// is not yet created.
	SelinuxEnabledByDefaultEnvKey = "DEFAULT_ENABLE_SELINUX"

	// SeccompProfileRecordHookAnnotationKey is the annotation on a Pod that
	// triggers the oci-seccomp-bpf-hook to trace the syscalls of a Pod and
	// created a seccomp profile.
	SeccompProfileRecordHookAnnotationKey = "io.containers.trace-syscall/"

	// SeccompProfileRecordLogsAnnotationKey is the annotation on a Pod that
	// triggers the internal log enricher to trace the syscalls of a Pod and
	// created a seccomp profile.
	SeccompProfileRecordLogsAnnotationKey = "io.containers.trace-logs/"

	// SeccompProfileRecordBpfAnnotationKey is the annotation on a Pod that
	// triggers the internal bpf module to trace the syscalls of a Pod and
	// created a seccomp profile.
	SeccompProfileRecordBpfAnnotationKey = "io.containers.trace-bpf/"

	// SelinuxProfileRecordLogsAnnotationKey is the annotation on a Pod that
	// triggers the internal log enricher to trace the AVC denials of a Pod and
	// created a selinux profile.
	SelinuxProfileRecordLogsAnnotationKey = "io.containers.trace-avcs/"

	// HealthProbePort is the port where the liveness probe will be served.
	HealthProbePort = 8085

	// AuditLogPath is the path to the auditd log file.
	AuditLogPath = "/var/log/audit/audit.log"

	// SyslogLogPath is the path to the syslog log file.
	SyslogLogPath = "/var/log/syslog"

	// LogEnricherProfile is the seccomp profile name for tracing syscalls from
	// the log enricher.
	LogEnricherProfile = "log-enricher-trace"

	// SelinuxPermissiveProfile is the selinux profile name for tracing AVC from
	// the log enricher.
	SelinuxPermissiveProfile = "selinuxrecording.process"

	// GRPCServerSocketMetrics is the socket path for the GRPC metrics server.
	GRPCServerSocketMetrics = "/var/run/grpc/metrics.sock"

	// GRPCServerSocketEnricher is the socket path for the GRPC enricher server.
	GRPCServerSocketEnricher = "/var/run/grpc/enricher.sock"

	// GRPCServerSocketBpfRecorder is the socket path for the GRPC bpf recorder server.
	GRPCServerSocketBpfRecorder = "/var/run/grpc/bpf-recorder.sock"
)

// ProfileRecordingOutputPath is the path where the recorded profiles will be
// stored. Those profiles are going to be reconciled into native CRDs and
// therefore have a limited lifetime.
var ProfileRecordingOutputPath = filepath.Join(os.TempDir(), "security-profiles-operator-recordings")

var ErrPodNamespaceEnvNotFound = errors.New("the env variable OPERATOR_NAMESPACE hasn't been set")

// GetOperatorNamespace gets the namespace that the operator is currently running on.
// Failure to get the namespace results in a panic.
func GetOperatorNamespace() string {
	ns, err := TryToGetOperatorNamespace()
	if err != nil {
		panic(err)
	}
	return ns
}

func TryToGetOperatorNamespace() (string, error) {
	// This is OPERATOR_NAMESPACE should have been set by the downward API to identify
	// the namespace which this controller is running from
	operatorNS := env.Default(OperatorNamespaceEnvKey, "")
	if operatorNS == "" {
		return "", ErrPodNamespaceEnvNotFound
	}
	return operatorNS, nil
}
