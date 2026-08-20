#!/bin/zsh

set -euo pipefail

repo_root="${0:A:h:h}"
env_file="$repo_root/.env"
profile="local-stdio"
listen_port="8080"
log_file="/private/tmp/m3-tunnel.log"
state_root="${REPOWORKER_STATE_DIR:-$HOME/.repoworker-state}"

if [[ "$state_root" != /* || "$state_root" == "$repo_root" || "$state_root" == "$repo_root"/* ]]; then
 print -u2 "REPOWORKER_STATE_DIR must be an absolute path outside the repository"
 exit 1
fi

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

mkdir -p "$state_root"
chmod 700 "$state_root"

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

# Override only the MCP command so the profile's credentials and tunnel
# identity remain unchanged while RepoWorker uses a fresh, explicit state root.
mcp_command="channel=main,command=$repo_root/bin/repoworker -repo-root $repo_root -state-dir $state_root -trusted-principal local-tunnel"
nohup tunnel-client run --profile "$profile" --mcp.command "$mcp_command" >>"$log_file" 2>&1 &
new_pid=$!
print "tunnel-client restarted (pid $new_pid)"

for _ in {1..50}; do
	if curl -fsS "http://127.0.0.1:$listen_port/readyz" >/dev/null 2>&1; then
		print "RepoWorker tunnel ready (state: $state_root)"
		exit 0
	fi
	if ! kill -0 "$new_pid" 2>/dev/null; then
		print -u2 "tunnel-client exited during startup; see $log_file"
		exit 1
	fi
	sleep 0.1
done

print -u2 "tunnel startup timed out; see $log_file"
exit 1
