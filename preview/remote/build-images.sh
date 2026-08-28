#!/usr/bin/env bash
set -euo pipefail

app_dir="${DURANTA_APP_DIR:-/opt/duranta-preview/app}"
cd "$app_dir"

postgis_context="$(mktemp -d)"
trap 'rm -rf -- "$postgis_context"' EXIT

podman build --layers -t localhost/duranta-preview/base:golden -f tools/base.dockerfile tools
podman build --layers -t localhost/duranta-preview/proto:golden -f proto/proto.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden proto
podman build --layers --target builder -t localhost/duranta-preview/backend:golden -f backend/backend.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden \
  --build-context proto=proto backend
podman build --layers --target frontend-build -t localhost/duranta-preview/frontend:golden -f frontend/frontend.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden \
  --build-context proto=proto frontend
cp -a tools/postgis/. "$postgis_context/"
chmod 0644 "$postgis_context/initdb-postgis.sh"
podman build --layers -t localhost/duranta-preview/postgis:golden "$postgis_context"
podman run --rm --entrypoint /bin/sh localhost/duranta-preview/postgis:golden \
  -ec 'test "$(stat -c %a /docker-entrypoint-initdb.d/10_postgis.sh)" = 644'
podman run --rm localhost/duranta-preview/postgis:golden postgres --version >/dev/null
podman build --layers -t localhost/duranta-preview/smspit:golden tools/smspit
