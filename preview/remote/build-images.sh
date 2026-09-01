#!/usr/bin/env bash
set -euo pipefail

app_dir="${DURANTA_APP_DIR:-/opt/duranta-preview/app}"
cd "$app_dir"
mode="${1:-all}"
cvml_image=localhost/duranta-preview/cvml:golden
cvml_input_label=com.duranta.preview.cvml-inputs

case "$mode" in
  all|cvml|cvml-fingerprint) ;;
  *) echo "Usage: duranta-preview-build-images [all|cvml|cvml-fingerprint]" >&2; exit 2 ;;
esac

cvml_input_fingerprint() {
  local base_id
  base_id="$(podman image inspect localhost/duranta-preview/base:golden --format '{{.Id}}')"
  {
    printf '%s\n' "$base_id"
    sha256sum \
      cvml/pyproject.toml \
      cvml/uv.lock \
      /usr/local/lib/duranta-preview/cvml.cpu.dockerfile
  } | sha256sum | cut -d ' ' -f 1
}

if [[ "$mode" == cvml-fingerprint ]]; then
  cvml_input_fingerprint
  exit 0
fi

temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
cvml_context="$temporary/cvml"
postgis_context="$temporary/postgis"
mkdir -p "$cvml_context" "$postgis_context"

if [[ "$mode" == all ]]; then
  podman build --layers -t localhost/duranta-preview/base:golden -f tools/base.dockerfile tools
  podman build --layers -t localhost/duranta-preview/proto:golden -f proto/proto.dockerfile \
    --build-context base=container-image://localhost/duranta-preview/base:golden proto
  podman build --layers --target builder -t localhost/duranta-preview/backend:golden -f backend/backend.dockerfile \
    --build-context base=container-image://localhost/duranta-preview/base:golden \
    --build-context proto=proto backend
  podman build --layers --target frontend-build -t localhost/duranta-preview/frontend:golden -f frontend/frontend.dockerfile \
    --build-context base=container-image://localhost/duranta-preview/base:golden \
    --build-context proto=proto frontend
fi

cvml_fingerprint="$(cvml_input_fingerprint)"
cp cvml/pyproject.toml cvml/uv.lock "$cvml_context/"
podman build --layers -t "$cvml_image" \
  --label "$cvml_input_label=$cvml_fingerprint" \
  -f /usr/local/lib/duranta-preview/cvml.cpu.dockerfile "$cvml_context"
podman run --rm --entrypoint /app/cvml/.venv/bin/python "$cvml_image" \
  -c 'import platform, torch, torchvision, yaml; assert platform.machine() == "aarch64"; assert not torch.cuda.is_available()'
test "$(podman image inspect "$cvml_image" --format '{{.Architecture}}')" = arm64

if [[ "$mode" == cvml ]]; then
  exit 0
fi

cp -a tools/postgis/. "$postgis_context/"
chmod 0644 "$postgis_context/initdb-postgis.sh"
podman build --layers -t localhost/duranta-preview/postgis:golden "$postgis_context"
podman run --rm --entrypoint /bin/sh localhost/duranta-preview/postgis:golden \
  -ec 'test "$(stat -c %a /docker-entrypoint-initdb.d/10_postgis.sh)" = 644'
podman run --rm localhost/duranta-preview/postgis:golden postgres --version >/dev/null
podman build --layers -t localhost/duranta-preview/smspit:golden tools/smspit
