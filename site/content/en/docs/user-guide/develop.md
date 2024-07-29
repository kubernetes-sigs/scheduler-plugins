---
weight: 3
---

# Developer guide

## Table of Contents

<!-- toc -->
- [How to build](#how-to-build)
- [How to debug](#how-to-debug)
- [How to start](#how-to-start)
- [Before submitting](#before-submitting)
<!-- /toc -->

## How to build

This section is about building on the local host, e.g. for debug purpose.
Your `GOPATH` environment variable should be set.
After cloning it into the `$GOPATH/src/sigs.k8s.io` directory
you can build an image by the following command:
```shell
make local-image
```
After that you'll take localhost:5000/scheduler-plugins/kube-scheduler:latest and
localhost:5000/scheduler-plugins/controller:latest images.

Your source code has to be located at `$GOPATH/src/sigs.k8s.io`, if you would like to change some type definition of the interfaces
and regenerate their implementation by running the command:
```shell
hack/update-codegen.sh
```

Also, if you are adding a new plugin args struct, to have it properly decoded, its name needs to follow the convention `<PluginName>Args`.

If you would like to build just the binaries you can do it by
```shell
make
```
command.
If you changed the branch or added new dependency you might need to regenerate vendor directory, for this you can use
```shell
make update-vendor
```
command.

## How to debug
By default, debug information is stripped, to keep it in the binary you need to remove -w command line option from
ldflags in the Makefile.
You can run all unit tests by **make unit-test** command or if you would like to run exact unit test
```shell
GOFLAGS="-v" go test ./pkg/util/podgroup.go ./pkg/util/constants.go ./pkg/util/podgroup_test.go
```
-v in GOFLAGS was used for verbosity
You can also run integration test locally, but etcd port 2379 on your system should be free.
```shell
make integration-test
```

## How to start
If you would like to start produced kube-scheduler image you can use it in your static kube-scheduler manifests or any kind of
deployment spec as following:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: schedulingplugin
  namespace: kube-system
spec:
  replicas: 1
  selector:
    matchLabels:
      component: scheduler
      tier: control-plane
  template:
    metadata:
      labels:
        component: scheduler
        tier: control-plane
    spec:
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
      containers:
        - image: localhost:5000/scheduler-plugins/kube-scheduler:latest
          imagePullPolicy: Never
          args:
          - --authentication-kubeconfig=/etc/kubernetes/scheduler.conf
          - --authorization-kubeconfig=/etc/kubernetes/scheduler.conf
          - --config=/etc/kubernetes/configs/scheduler-config.yaml
          - -v=9
          name: schedulingplugin
          securityContext:
            privileged: true
          volumeMounts:
          - mountPath: /etc/kubernetes
            name: etckubernetes
      hostNetwork: false
      hostPID: false
      volumes:
      - hostPath:
          path: /etc/kubernetes/
          type: Directory
        name: etckubernetes
```
Where example for scheduler-config.yaml, could be taken from manifests/*/scheduler-config.yaml.


## Before submitting
In addition to starting integration and unit tests, check formatting
```shell
make verify
```
