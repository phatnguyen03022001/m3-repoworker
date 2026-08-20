#!/bin/zsh

set -euo pipefail

repo_root="${0:A:h:h}"
fixture_root="$(mktemp -d /private/tmp/m3-repoworker-cold-cache.XXXXXX)"
trap 'chmod -R u+rwX "$fixture_root" 2>/dev/null || true; rm -rf "$fixture_root"' EXIT

cache_root="$fixture_root/go-mod"
build_root="$fixture_root/go-build"
path_root="$fixture_root/go-path"
mkdir -p "$cache_root" "$build_root" "$path_root"

print "controlled bootstrap into $fixture_root"
(cd "$repo_root" && make bootstrap GOMODCACHE="$cache_root" GOCACHE="$build_root" GOPATH="$path_root")

print "offline deterministic verification"
(cd "$repo_root" && make offline-verify GOMODCACHE="$cache_root" GOCACHE="$build_root" GOPATH="$path_root")

print "sequential production repo_verify presets"
(cd "$repo_root" && REPOWORKER_RUN_PRESET_SEQUENCE=1 GOPROXY=off GOSUMDB=off GOMODCACHE="$cache_root" GOCACHE="$build_root" GOPATH="$path_root" go test ./cmd/repoworker -run '^TestProductionRepoVerifyPresetsSequentially$' -count=1)

print "cold-cache verification passed"
