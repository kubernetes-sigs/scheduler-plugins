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

```bash
# Run hot reload script
./hot-reload.sh mycluster
```
