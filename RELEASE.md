# Release Guide

## Semi-automatic

1. Make sure your repo is clean by git's standards
2. Create a release branch `git checkout -b release-1.19` (not required for patch releases)
3. Push the release branch to the scheduler-plugins repo and ensure branch protection is enabled (not required for patch releases)
4. Tag the repository from the `master` branch (from the `release-1.19` branch for a patch release) and push the tag `VERSION=v0.19.0 git tag -m $VERSION $VERSION; git push origin $VERSION`
5. Publish a draft release using the tag you just created
6. Perform the [image promotion process](https://github.com/kubernetes/k8s.io/tree/main/k8s.gcr.io#image-promoter)
7. Publish release
8. Email `kubernetes-sig-scheduling@googlegroups.com` to announce the release

## Manual

1. Make sure your repo is clean by git's standards
2. Create a release branch `git checkout -b release-1.19` (not required for patch releases)
3. Push the release branch to the scheduler-plugins repo and ensure branch protection is enabled (not required for patch releases)
4. Tag the repository from the `master` branch (from the `release-1.19` branch for a patch release) and push the tag `VERSION=v0.19.0 git tag -m $VERSION $VERSION; git push origin $VERSION`
5. Checkout the tag you just created and make sure your repo is clean by git's standards `git checkout $VERSION`
6. Build and push the container image to the staging registry `RELEASE_VERSION=$VERSION make push-release-image`
7. Publish a draft release using the tag you just created
8. Perform the [image promotion process](https://github.com/kubernetes/k8s.io/tree/main/k8s.gcr.io#image-promoter)
9. Publish release
10. Email `kubernetes-sig-scheduling@googlegroups.com` to announce the release

## Notes
See [post-scheduler-plugins-push-images dashboard](https://testgrid.k8s.io/sig-scheduling#post-scheduler-plugins-push-images) for staging registry image build job status.

View the scheduler-plugins staging registry using [this URL](https://console.cloud.google.com/gcr/images/k8s-staging-scheduler-plugins/GLOBAL) in a web browser
or use the below `gcloud` commands.

List images in staging registry.
```
gcloud container images list --repository gcr.io/k8s-staging-scheduler-plugins
```

List kube-scheduler image tags in the staging registry.
```
gcloud container images list-tags gcr.io/k8s-staging-scheduler-plugins/kube-scheduler
```

Get SHA256 hash for a specific image in the staging registry.
```
gcloud container images describe gcr.io/k8s-staging-scheduler-plugins/kube-scheduler:v20200206-0.9.0-94-ge2a23f284
```

Pull image from the staging registry.
```
docker pull gcr.io/k8s-staging-scheduler-plugins/kube-scheduler:v20200206-0.9.0-94-ge2a23f284
```
