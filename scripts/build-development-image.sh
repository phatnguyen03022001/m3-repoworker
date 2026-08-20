#!/bin/zsh

set -euo pipefail

repo_root="${0:A:h:h}"
tag="${REPOWORKER_DEVELOPMENT_IMAGE:-repoworker-dev:local}"

if [[ "$tag" == *$'\n'* || "$tag" == *$'\r'* || "$tag" == *' '* || "$tag" == '' ]]; then
  print -u2 "REPOWORKER_DEVELOPMENT_IMAGE must be a non-empty image tag without whitespace"
  exit 1
fi

container_binary="$(command -v container || true)"
if [[ -z "$container_binary" ]]; then
  print -u2 "Apple container CLI is required to build $tag"
  exit 1
fi

build_context="$(mktemp -d /private/tmp/m3-repoworker-development-image.XXXXXX)"
trap 'rm -rf "$build_context"' EXIT

cp "$repo_root/container/Containerfile.development" "$build_context/Containerfile"
cp "$repo_root/go.mod" "$build_context/go.mod"
cp "$repo_root/go.sum" "$build_context/go.sum"

print "building container-confined development image $tag"
"$container_binary" build \
  --file "$build_context/Containerfile" \
  --tag "$tag" \
  "$build_context"
print "development image ready: $tag"
