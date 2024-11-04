# Getting Started

## Configuring load-watcher on a Kubernetes cluster

#### Clone repository.

We use the available manifest file to create load-watcher deployment and service CRDs.

```bash
git clone https://github.com/paypal/load-watcher.git
```

Building image

```bash
docker build -t load-watcher:local .
docker tag load-watcher:local <replace_with_image_registry>/loadwatcher:latest
docker push <replace_with_image_registry>/loadwatcher:latest
```

Deploy load-watcher
Follow https://github.com/paypal/load-watcher?tab=readme-ov-file#deploy-load-watcher-as-a-service

## Configure PEAKS scheduler plugin

Building image

```bash
make release-image.amd64
docker tag <image tag> <new image tag>
docker push <new image tag>
```

Configuring power model

```bash
kubectl create configmap peaks-node-power-model --from-file=peaks-power-model-config.json -n kube-system
```

Deploy crds

```bash
kubectl apply -f peaks-crd.yaml
```

Deploy peaks plugin

```bash
kubectl apply -f deployment.yaml
```

Test peaks plugin

```bash
kubectl apply -f test-po.yaml
```

## Running the unit test cases
To run the `go` unit test cases, set the environment variable `NODE_POWER_MODEL` appropriately, as below example.
```bash
export NODE_POWER_MODEL=./power_model/node_power_model
```
