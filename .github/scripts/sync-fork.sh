#!/usr/bin/env bash
set -euo pipefail

upstream_sha=$(git rev-parse --verify "${1:?upstream commit required}^{commit}")
fork_sha=$(git rev-parse HEAD)

if git merge-base --is-ancestor "$upstream_sha" "$fork_sha"; then
  echo "Fork already contains the upstream revision"
  exit 0
fi

git merge-base "$fork_sha" "$upstream_sha" >/dev/null
provider_commits=$(git rev-list --full-history "$fork_sha..$upstream_sha" -- terraform/provider/)
if [[ -n "$provider_commits" ]]; then
  echo "::error::Upstream changed terraform/provider/. Review those commits before syncing"
  printf '%s\n' "$provider_commits"
  exit 1
fi

git merge --no-edit "$upstream_sha"
git diff --exit-code "$fork_sha" HEAD -- terraform/provider/
