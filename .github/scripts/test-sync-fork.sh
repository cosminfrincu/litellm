#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/sync-fork.sh"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

git_work() {
  git -C "$work" "$@" >/dev/null 2>&1
}

for scenario in outside_provider provider reverted_provider unchanged conflict; do
  work="$temporary/$scenario"
  git init --quiet --initial-branch=main "$work"
  git_work config user.name "Sync test"
  git_work config user.email "sync-test@example.invalid"
  git_work config commit.gpgsign false
  git_work config core.hooksPath /dev/null
  mkdir -p "$work/terraform/provider"
  printf 'base provider\n' >"$work/terraform/provider/main.go"
  printf 'base readme\n' >"$work/README.md"
  git_work add .
  git_work commit -m base
  base=$(git -C "$work" rev-parse HEAD)

  printf 'fork customization\n' >"$work/terraform/provider/fork.go"
  if [[ "$scenario" == conflict ]]; then
    printf 'fork readme\n' >"$work/README.md"
  fi
  git_work add .
  git_work commit -m fork
  original=$(git -C "$work" rev-parse HEAD)

  git_work switch -c upstream "$base"
  case "$scenario" in
    provider | reverted_provider)
      printf 'upstream provider\n' >"$work/terraform/provider/main.go"
      ;;
    outside_provider | conflict)
      printf 'upstream readme\n' >"$work/README.md"
      ;;
  esac
  if [[ "$scenario" != unchanged ]]; then
    git_work add .
    git_work commit -m upstream
  fi
  if [[ "$scenario" == reverted_provider ]]; then
    git_work revert --no-edit HEAD
  fi
  upstream=$(git -C "$work" rev-parse HEAD)
  git_work switch main

  if (cd "$work" && bash "$script" "$upstream") >"$temporary/$scenario.log" 2>&1; then
    case "$scenario" in
      outside_provider | unchanged) ;;
      *) cat "$temporary/$scenario.log"; exit 1 ;;
    esac
    git -C "$work" merge-base --is-ancestor "$upstream" HEAD
    [[ "$(git -C "$work" show HEAD:terraform/provider/fork.go)" == "fork customization" ]] || exit 1
    [[ "$(git -C "$work" show HEAD:terraform/provider/main.go)" == "base provider" ]] || exit 1
    if [[ "$scenario" == outside_provider ]]; then
      [[ "$(git -C "$work" show HEAD:README.md)" == "upstream readme" ]] || exit 1
    fi
  else
    case "$scenario" in
      outside_provider | unchanged) cat "$temporary/$scenario.log"; exit 1 ;;
    esac
    [[ "$(git -C "$work" rev-parse HEAD)" == "$original" ]] || exit 1
  fi
  echo "PASS $scenario"
done
