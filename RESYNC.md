# Maintaining openshift-kni/scheduler-plugins

openshift-kni/scheduler-plugins is based on upstream kubernetes-sigs/scheduler-plugins.
With every release of kubernetes-sigs/scheduler-plugins, it is necessary to incorporate the upstream changes
while ensuring that our downstream customizations are maintained.

Nonetheless, we have the freedom to choose if we want this changes at all, because there are times when the upstream
changes are not relevant for our work.

## Preparing the local repo clone
Clone from a personal fork of openshift-kni/scheduler-plugins via a pushable (ssh) url:

`git clone git@github.com:openshift-kni/scheduler-plugins.git`

Add a remote for upstream and fetch its branches:

`git remote add --fetch upstream https://github.com/kubernetes-sigs/scheduler-plugins`

## Creating a new local branch for the new resync

Branch the target openshift-kni/scheduler master branch to a new resync local branch 

`git checkout master`

`git checkout -b "resync-$(date +%Y%m%d)"`

## Merge changes from upstream

`git merge upstream/master`

fix conflicts introduced by kni-local changes and send PR for review

## Patching openshift-kni specific commits

Every commit that is openshift-kni/scheduler-plugins specific should have a prefix of [KNI] 
at the beginning of the commit message.

## Document changes

For the sake of transparency, for every resync process we should update the following table:

| Resync Date | Merge With Upstream Tag/Commit                                                                       | Author |
|-------------|------------------------------------------------------------------------------------------------------|--------|
| 12.11.2021  | https://github.com/kubernetes-sigs/scheduler-plugins/commit/b8d13e17a3e1f633d72d71276a3da6fecf89f2e3 | Tal-or |

The newest resync should appear in the first row. 