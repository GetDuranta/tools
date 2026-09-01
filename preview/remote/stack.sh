#!/usr/bin/env bash
set -euo pipefail

runtime_dir="${DURANTA_RUNTIME_DIR:-/opt/duranta-preview/runtime}"
app_dir="${DURANTA_APP_DIR:-/opt/duranta-preview/app}"
env_file="$runtime_dir/compose.env"
compose_file="/usr/local/lib/duranta-preview/compose.preview.yml"
project_name="duranta-preview"
cvml_models="$runtime_dir/cvml-models.cpu.yaml"
cvml_fingerprint_file="$runtime_dir/cvml-fingerprint"
cvml_image=localhost/duranta-preview/cvml:golden

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

prepare_cvml_runtime() {
  local lfs_path model_count=0
  while IFS= read -r lfs_path; do
    case "$lfs_path" in
      cvml/model_artifacts/*)
        model_count=$((model_count + 1))
        if [[ ! -s "$app_dir/$lfs_path" ]]; then
          echo "Missing CVML model artifact: $lfs_path" >&2
          return 1
        fi
        if head -c 128 "$app_dir/$lfs_path" | grep -Fq 'version https://git-lfs.github.com/spec/v1'; then
          echo "CVML model artifact is still an LFS pointer: $lfs_path" >&2
          return 1
        fi
        ;;
    esac
  done < <(git -C "$app_dir" lfs ls-files --name-only)
  ((model_count > 0)) || { echo "No CVML LFS model artifacts found" >&2; return 1; }

  node /usr/local/lib/duranta-preview/prepare-cvml-models.mjs \
    "$app_dir/cvml/algorithm/models.yaml" \
    "$cvml_models"
}

cvml_fingerprint() {
  local image_id
  image_id="$(podman image inspect localhost/duranta-preview/cvml:golden --format '{{.Id}}')"
  {
    git -C "$app_dir" rev-parse HEAD:cvml HEAD:proto/python
    printf '%s\n' "$image_id"
    sha256sum "$cvml_models"
  } | sha256sum | cut -d ' ' -f 1
}

ensure_cvml_image() {
  local actual desired
  desired="$(/usr/local/bin/duranta-preview-build-images cvml-fingerprint)"
  actual="$(podman image inspect "$cvml_image" \
    --format '{{ index .Labels "com.duranta.preview.cvml-inputs" }}' 2>/dev/null || true)"
  if [[ "$actual" == "$desired" ]]; then
    return 0
  fi

  # Free the model's memory before a dependency cache miss on a 32 GiB host.
  "${compose[@]}" stop cvml >/dev/null 2>&1 || true
  /usr/local/bin/duranta-preview-build-images cvml
  actual="$(podman image inspect "$cvml_image" \
    --format '{{ index .Labels "com.duranta.preview.cvml-inputs" }}')"
  [[ "$actual" == "$desired" ]] || {
    echo "CVML image input fingerprint does not match the checkout" >&2
    return 1
  }
}

wait_for_cvml() {
  local deadline=$((SECONDS + 1200))
  while ((SECONDS < deadline)); do
    if curl -fsS --max-time 2 http://127.0.0.1:18082/ping >/dev/null; then
      cvml_fingerprint >"$cvml_fingerprint_file"
      return 0
    fi
    sleep 2
  done
  echo "Timed out waiting for CVML readiness" >&2
  "${compose[@]}" logs --tail=200 cvml >&2 || true
  return 1
}

ensure_cvml() {
  local previous_fingerprint next_fingerprint needs_restart=false
  prepare_cvml_runtime
  ensure_cvml_image
  previous_fingerprint="$(cat "$cvml_fingerprint_file" 2>/dev/null || true)"
  next_fingerprint="$(cvml_fingerprint)"
  if [[ "$previous_fingerprint" != "$next_fingerprint" ]]; then
    needs_restart=true
  fi
  if [[ -n "$(git -C "$app_dir" status --porcelain --untracked-files=all -- cvml proto/python)" ]]; then
    needs_restart=true
  fi

  if [[ "$needs_restart" == true ]]; then
    "${compose[@]}" up -d --no-build --no-deps --force-recreate cvml
  else
    "${compose[@]}" up -d --no-build --no-deps cvml
  fi
  wait_for_cvml
}

set_frontend_mode() {
  local mode="$1" temporary
  ensure_cvml
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
    prepare_cvml_runtime
    ensure_cvml_image
    "${compose[@]}" up -d --no-build --remove-orphans
    wait_for_url https://127.0.0.1:18443/healthcheck
    wait_for_url https://127.0.0.1:18443/a/
    wait_for_url http://127.0.0.1:13001/oidc/.well-known/openid-configuration
    wait_for_url http://127.0.0.1:13443/
    wait_for_cvml
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
    # A full build can peak high enough to OOM beside the 16+ GiB model worker.
    "${compose[@]}" stop cvml >/dev/null 2>&1 || true
    /usr/local/bin/duranta-preview-build-images
    ensure_cvml
    "${compose[@]}" up -d --no-build --remove-orphans
    ;;
  ensure-cvml)
    ensure_cvml
    ;;
  frontend-dev)
    set_frontend_mode dev
    ;;
  frontend-production)
    set_frontend_mode production
    ;;
  *)
    echo "Usage: duranta-preview-stack {up|down|status|logs|rebuild|ensure-cvml|frontend-dev|frontend-production}" >&2
    exit 2
    ;;
esac
