# MyPlugin

## Overview

MyPlugin is a custom Kubernetes scheduler plugin that demonstrates how to create custom scheduling logic.

## Installation and Setup (Single Scheduler Mode)

This setup **replaces** the default Kubernetes scheduler with MyPlugin scheduler, providing unified scheduling without resource conflicts.

### 🔧 Prerequisites

**For Windows users (as this project was tested on):**

- WSL2 with Ubuntu distribution
- Docker Desktop with WSL2 integration enabled
- Kind installed in WSL: `go install sigs.k8s.io/kind@latest`
- kubectl installed in WSL
- Go 1.24+ installed

### Setup kind Cluster using provided bash script

```bash
# On WSL (required for Windows users)
wsl ./setup-cluster.sh mycluster
```

NOTE: Can be necessary to convert to Unix line endings. Use `dos2unix` or similar tools.

```bash
# Convert to Unix line endings
dos2unix setup-cluster.sh
```

### 🔥 Hot Reload Development

The biggest advantage of our setup is **hot reload** functionality, which reduces development time.
Hot reload updates the plugin code and restarts the scheduler pod.

#### Why Hot Reload?

**Traditional method:**

1. Change Go code
2. `make local-image`
3. `kind load docker-image`
4. Manually update static pod manifest

**Hot reload method:

1. Change Go code
2. `docker build` with Docker cache
3. `kind load docker-image`
4. `update static pod manifest`
5. Scheduler pod restart

#### How to use Hot Reload?

1. Make some change in `pkg/myplugin/myplugin.go`
2. Run the hot reload script:

```bash
# Run hot reload script
./hot-reload.sh mycluster
```

3. Run a test pod to trigger MyPlugin:

```bash
kubectl run test-v5 --image=nginx --context kind-mycluster
```

4. Check MyPlugin logs of the scheduler pod:

```bash
kubectl logs -n kube-system -l component=kube-scheduler --context kind-mycluster --tail=20 | grep "MyPlugin"
```

NOTE: Can be necessary to convert to Unix line endings. Use `dos2unix` or similar tools.

```bash
# Convert to Unix line endings
dos2unix hot-reload.sh
```
