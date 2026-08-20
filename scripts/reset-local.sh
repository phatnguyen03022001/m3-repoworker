#!/bin/zsh

set -euo pipefail

repo_root="${0:A:h:h}"
state_root="${REPOWORKER_STATE_DIR:-$HOME/.repoworker-state}"
timestamp="$(date +%Y%m%d-%H%M%S)"

if [[ "$state_root" != /* || "$state_root" == "$repo_root" || "$state_root" == "$repo_root"/* ]]; then
	print -u2 "REPOWORKER_STATE_DIR must be an absolute path outside the repository"
	exit 1
fi

if [[ -e "$state_root" ]]; then
	backup_root="${state_root}.backup.${timestamp}"
	mv "$state_root" "$backup_root"
	print "moved old state to $backup_root"
fi

mkdir -p "$state_root"
chmod 700 "$state_root"

print "running verification and rebuilding RepoWorker"
(cd "$repo_root" && make verify)

REPOWORKER_STATE_DIR="$state_root" "$repo_root/scripts/restart-local-tunnel.sh"
print "local RepoWorker reset complete"
