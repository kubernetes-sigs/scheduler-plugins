module sigs.k8s.io/scheduler-plugins

go 1.20

require (
	github.com/diktyo-io/appgroup-api v1.0.1-alpha
	github.com/diktyo-io/networktopology-api v1.0.1-alpha
	github.com/dustin/go-humanize v1.0.0
	github.com/go-logr/logr v1.2.4
	github.com/google/go-cmp v0.5.9
	github.com/k8stopologyawareschedwg/noderesourcetopology-api v0.1.1
	github.com/k8stopologyawareschedwg/podfingerprint v0.2.2
	github.com/patrickmn/go-cache v2.1.0+incompatible
	github.com/paypal/load-watcher v0.2.3
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.8.1
	gonum.org/v1/gonum v0.12.0
	k8s.io/api v0.27.4
	k8s.io/apimachinery v0.27.4
	k8s.io/apiserver v0.27.4
	k8s.io/client-go v0.27.4
	k8s.io/code-generator v0.27.4
	k8s.io/component-base v0.27.4
	k8s.io/component-helpers v0.27.4
	k8s.io/klog/hack/tools v0.0.0-20210917071902-331d2323a192
	k8s.io/klog/v2 v2.90.1
	k8s.io/kube-scheduler v0.27.4
	k8s.io/kubernetes v1.27.4
	k8s.io/utils v0.0.0-20230209194617-a36077c30491
	sigs.k8s.io/controller-runtime v0.15.0
	sigs.k8s.io/yaml v1.3.0
)

require (
	cloud.google.com/go/compute/metadata v0.2.3 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20210617225240-d185dfc1b5a1 // indirect
	github.com/NYTimes/gziphandler v1.1.1 // indirect
	github.com/OneOfOne/xxhash v1.2.8 // indirect
	github.com/antlr/antlr4/runtime/Go/antlr v1.4.10 // indirect
	github.com/asaskevich/govalidator v0.0.0-20190424111038-f61b66f89f4a // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/cenkalti/backoff/v4 v4.1.3 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/coreos/go-semver v0.3.0 // indirect
	github.com/coreos/go-systemd/v22 v22.4.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/distribution v2.8.1+incompatible // indirect
	github.com/emicklei/go-restful/v3 v3.9.0 // indirect
	github.com/evanphx/json-patch v4.12.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.6.0 // indirect
	github.com/felixge/httpsnoop v1.0.3 // indirect
	github.com/francoispqt/gojay v1.2.13 // indirect
	github.com/fsnotify/fsnotify v1.6.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.19.6 // indirect
	github.com/go-openapi/jsonreference v0.20.1 // indirect
	github.com/go-openapi/swag v0.22.3 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/cel-go v0.12.6 // indirect
	github.com/google/gnostic v0.5.7-v3refs // indirect
	github.com/google/gofuzz v1.1.0 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.7.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/inconshreveable/mousetrap v1.0.1 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/jpillora/backoff v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/mitchellh/mapstructure v1.4.1 // indirect
	github.com/moby/sys/mountinfo v0.6.2 // indirect
	github.com/moby/term v0.0.0-20221205130635-1aeaba878587 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/mwitkow/go-conntrack v0.0.0-20190716064945-2f068394615f // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/selinux v1.10.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_golang v1.15.1 // indirect
	github.com/prometheus/client_model v0.4.0 // indirect
	github.com/prometheus/common v0.42.0 // indirect
	github.com/prometheus/procfs v0.9.0 // indirect
	github.com/sirupsen/logrus v1.9.0 // indirect
	github.com/spf13/cobra v1.6.0 // indirect
	github.com/stoewer/go-strcase v1.2.0 // indirect
	go.etcd.io/etcd/api/v3 v3.5.7 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.7 // indirect
	go.etcd.io/etcd/client/v3 v3.5.7 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.35.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.35.1 // indirect
	go.opentelemetry.io/otel v1.10.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/internal/retry v1.10.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.10.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.10.0 // indirect
	go.opentelemetry.io/otel/metric v0.31.0 // indirect
	go.opentelemetry.io/otel/sdk v1.10.0 // indirect
	go.opentelemetry.io/otel/trace v1.10.0 // indirect
	go.opentelemetry.io/proto/otlp v0.19.0 // indirect
	go.uber.org/atomic v1.7.0 // indirect
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.24.0 // indirect
	golang.org/x/crypto v0.1.0 // indirect
	golang.org/x/exp v0.0.0-20210220032938-85be41e4509f // indirect
	golang.org/x/mod v0.10.0 // indirect
	golang.org/x/net v0.10.0 // indirect
	golang.org/x/oauth2 v0.5.0 // indirect
	golang.org/x/sync v0.2.0 // indirect
	golang.org/x/sys v0.8.0 // indirect
	golang.org/x/term v0.8.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	golang.org/x/time v0.3.0 // indirect
	golang.org/x/tools v0.9.1 // indirect
	gomodules.xyz/jsonpatch/v2 v2.3.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20221202195650-67e5cbc046fd // indirect
	google.golang.org/grpc v1.51.0 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.0.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apiextensions-apiserver v0.27.4 // indirect
	k8s.io/cloud-provider v0.27.4 // indirect
	k8s.io/controller-manager v0.27.4 // indirect
	k8s.io/csi-translation-lib v0.27.4 // indirect
	k8s.io/dynamic-resource-allocation v0.27.4 // indirect
	k8s.io/gengo v0.0.0-20220902162205-c0856e24416d // indirect
	k8s.io/kms v0.27.4 // indirect
	k8s.io/kube-openapi v0.0.0-20230501164219-8b0f38b5fd1f // indirect
	k8s.io/kubelet v0.27.4 // indirect
	k8s.io/metrics v0.27.4 // indirect
	k8s.io/mount-utils v0.27.4 // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.1.2 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
)

replace (
	k8s.io/api => k8s.io/api v0.27.4
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.27.4
	k8s.io/apimachinery => k8s.io/apimachinery v0.27.4
	k8s.io/apiserver => k8s.io/apiserver v0.27.4
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.27.4
	k8s.io/client-go => k8s.io/client-go v0.27.4
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.27.4
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.27.4
	k8s.io/code-generator => k8s.io/code-generator v0.27.4
	k8s.io/component-base => k8s.io/component-base v0.27.4
	k8s.io/component-helpers => k8s.io/component-helpers v0.27.4
	k8s.io/controller-manager => k8s.io/controller-manager v0.27.4
	k8s.io/cri-api => k8s.io/cri-api v0.27.4
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.27.4
	k8s.io/dynamic-resource-allocation => k8s.io/dynamic-resource-allocation v0.27.4
	k8s.io/kms => k8s.io/kms v0.27.4
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.27.4
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.27.4
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.27.4
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.27.4
	k8s.io/kubectl => k8s.io/kubectl v0.27.4
	k8s.io/kubelet => k8s.io/kubelet v0.27.4
	k8s.io/kubernetes => k8s.io/kubernetes v1.27.4
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.27.4
	k8s.io/metrics => k8s.io/metrics v0.27.4
	k8s.io/mount-utils => k8s.io/mount-utils v0.27.4
	k8s.io/pod-security-admission => k8s.io/pod-security-admission v0.27.4
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.27.4
)
