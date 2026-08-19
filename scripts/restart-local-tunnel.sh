#!/bin/zsh

set -euo pipefail

repo_root="${0:A:h:h}"
env_file="$repo_root/.env"
profile="local-stdio"
listen_port="8080"
log_file="/private/tmp/m3-tunnel.log"

if [[ ! -r "$env_file" ]]; then
	print -u2 "missing readable .env"
	exit 1
fi

# Load credentials into this process only. Never echo the environment or key.
set -a
source "$env_file"
set +a
if [[ -z "${CONTROL_PLANE_API_KEY:-}" ]]; then
	print -u2 "CONTROL_PLANE_API_KEY is missing"
	exit 1
fi

old_pid="$(lsof -tiTCP:"$listen_port" -sTCP:LISTEN -c tunnel-client 2>/dev/null | head -n1 || true)"
if [[ -n "$old_pid" ]]; then
	kill "$old_pid"
	for _ in {1..50}; do
		if ! kill -0 "$old_pid" 2>/dev/null; then
			break
		fi
		sleep 0.1
	done
	if kill -0 "$old_pid" 2>/dev/null; then
		kill -KILL "$old_pid"
	fi
fi

nohup tunnel-client run --profile "$profile" >>"$log_file" 2>&1 &
new_pid=$!
print "tunnel-client restarted (pid $new_pid)"
