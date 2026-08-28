#!/usr/bin/env bash
set -euo pipefail

runtime_dir="${DURANTA_RUNTIME_DIR:-/opt/duranta-preview/runtime}"
env_file="$runtime_dir/compose.env"
compose_file="/usr/local/lib/duranta-preview/compose.preview.yml"

[[ -f "$env_file" ]] || { echo "Missing $env_file" >&2; exit 1; }
export PODMAN_COMPOSE_PROVIDER=/usr/local/bin/docker-compose
compose=(podman compose --env-file "$env_file" -f "$compose_file")

wait_for_url() {
  local url="$1"
  local attempts="${2:-180}"
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if curl -kfsS --max-time 2 "$url" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  echo "Timed out waiting for $url" >&2
  return 1
}

case "${1:-}" in
  up)
    "${compose[@]}" up -d --no-build --remove-orphans
    wait_for_url https://127.0.0.1:18443/healthcheck
    wait_for_url http://127.0.0.1:13001/oidc/.well-known/openid-configuration
    wait_for_url http://127.0.0.1:13443/
    ;;
  down)
    "${compose[@]}" down
    ;;
  status)
    "${compose[@]}" ps
    ;;
  logs)
    shift
    "${compose[@]}" logs --tail=200 "$@"
    ;;
  rebuild)
    /usr/local/bin/duranta-preview-build-images
    "${compose[@]}" up -d --no-build --remove-orphans
    ;;
  *)
    echo "Usage: duranta-preview-stack {up|down|status|logs|rebuild}" >&2
    exit 2
    ;;
esac
