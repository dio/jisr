#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
spa_dir="$(cd "$script_dir/.." && pwd)"

envoy_bin="${ENVOY_BIN:-envoy}"
admin_url="${SPA_ENVOY_ADMIN_URL:-http://127.0.0.1:9901}"

npm ci --prefix "$script_dir"
make -C "$spa_dir" build

(
	cd "$spa_dir"
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH="$spa_dir" "$envoy_bin" -c envoy.yaml --log-level warning
) &
envoy_pid=$!
trap 'kill "$envoy_pid"' EXIT

ready=
for _ in {1..50}; do
	if curl -fsS "$admin_url/ready" >/dev/null; then
		ready=1
		break
	fi
	sleep 0.2
done
test "$ready" = "1"

make -C "$spa_dir" e2e
