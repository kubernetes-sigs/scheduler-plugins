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