#!/bin/bash

set -e -o pipefail

function finish {
	if [ -f "$commit_msg_filename" ]; then
		rm -f "$commit_msg_filename"
	fi
}
trap finish EXIT

if [[ "$TRIGGER_BRANCH" = resync-* ]]; then
  echo "WARN: resync branch no commit enforcement will be triggered"
  exit 0
fi

if [[ -z "$UPSTREAM_COMMIT" ]]; then
	# CI=true is set by prow as a way to detect we are running under the ci
	if [[ -n "$CI" ]]; then
		latest_upstream_commit=$(curl -H "Accept: application/vnd.github.v3+json" https://api.github.com/repos/openshift-kni/scheduler-plugins/commits?per_page=1 | jq -r '.[0].sha')
	else
		if [[ -z "$UPSTREAM_BRANCH" ]]; then
			latest_upstream_commit="origin/master"
		else
			latest_upstream_commit=$UPSTREAM_BRANCH
		fi

		if [[ ! $(git branch --list --all "$latest_upstream_commit") ]]; then
			echo WARN: could not find "$latest_upstream_commit", consider using a different UPSTREAM_BRANCH value
		fi

	fi
else
	latest_upstream_commit="$UPSTREAM_COMMIT"
	if [[ ! $(git cat-file -t "$UPSTREAM_COMMIT") == "commit" ]]; then
		echo WARN: "$UPSTREAM_COMMIT" commitish could not be found in repo
	fi
fi

commits_diff_count=$(git log --oneline "$latest_upstream_commit"..HEAD | wc -l)
if [[ $commits_diff_count -eq 0 ]]; then
	echo "WARN: no changes detected"
	exit 0
fi

echo commits between "$latest_upstream_commit"..HEAD:
echo "---"
git log --oneline --no-merges "$latest_upstream_commit"..HEAD
echo "---"

# list commits
for commitish in $( git log --oneline --no-merges "$latest_upstream_commit"..HEAD | cut -d' ' -f 1); do
  echo "CHECK: $commitish"
  .github/hooks/commit-msg $( git log --format=%s -n 1 "$commitish" )
  if [[ "$?" != "0" ]]; then
    echo "-> FAIL: $commitish"
    exit 20
  fi
  echo "-> PASS: $commitish"
done

