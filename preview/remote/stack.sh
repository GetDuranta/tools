#!/usr/bin/env bash
set -euo pipefail

runtime_dir="${DURANTA_RUNTIME_DIR:-/opt/duranta-preview/runtime}"
env_file="$runtime_dir/compose.env"
compose_file="/usr/local/lib/duranta-preview/compose.preview.yml"
project_name="duranta-preview"

[[ -f "$env_file" ]] || { echo "Missing $env_file" >&2; exit 1; }
export PODMAN_COMPOSE_PROVIDER=/usr/local/bin/docker-compose
compose=(podman compose --project-name "$project_name" --env-file "$env_file" -f "$compose_file")

remove_stack() {
  local attempt ids network
  network="${project_name}_duranta"

  for attempt in 1 2 3; do
    if "${compose[@]}" down; then
      break
    fi
    sleep "$attempt"
  done

  mapfile -t ids < <(podman ps -aq --filter "label=com.docker.compose.project=$project_name")
  if (( ${#ids[@]} > 0 )) || podman network exists "$network"; then
    echo "Preview containers or network remain after shutdown" >&2
    return 1
  fi
}

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

set_frontend_mode() {
  local mode="$1" temporary
  if grep -q '^PREVIEW_FRONTEND_MODE=' "$env_file"; then
    temporary="$(mktemp)"
    sed "s/^PREVIEW_FRONTEND_MODE=.*/PREVIEW_FRONTEND_MODE=$mode/" "$env_file" >"$temporary"
    cat "$temporary" >"$env_file"
    rm -f "$temporary"
  else
    printf '\nPREVIEW_FRONTEND_MODE=%s\n' "$mode" >>"$env_file"
  fi
  "${compose[@]}" up -d --no-deps --force-recreate frontend
}

case "${1:-}" in
  up)
    "${compose[@]}" up -d --no-build --remove-orphans
    wait_for_url https://127.0.0.1:18443/healthcheck
    wait_for_url https://127.0.0.1:18443/a/
    wait_for_url http://127.0.0.1:13001/oidc/.well-known/openid-configuration
    wait_for_url http://127.0.0.1:13443/
    ;;
  down)
    remove_stack
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
  frontend-dev)
    set_frontend_mode dev
    ;;
  frontend-production)
    set_frontend_mode production
    ;;
  *)
    echo "Usage: duranta-preview-stack {up|down|status|logs|rebuild|frontend-dev|frontend-production}" >&2
    exit 2
    ;;
esac
