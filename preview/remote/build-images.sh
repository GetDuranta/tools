#!/usr/bin/env bash
set -euo pipefail

app_dir="${DURANTA_APP_DIR:-/opt/duranta-preview/app}"
cd "$app_dir"

podman build --layers -t localhost/duranta-preview/base:golden -f tools/base.dockerfile tools
podman build --layers -t localhost/duranta-preview/proto:golden -f proto/proto.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden proto
podman build --layers --target builder -t localhost/duranta-preview/backend:golden -f backend/backend.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden \
  --build-context proto=proto backend
podman build --layers --target frontend-build -t localhost/duranta-preview/frontend:golden -f frontend/frontend.dockerfile \
  --build-context base=container-image://localhost/duranta-preview/base:golden \
  --build-context proto=proto frontend
podman build --layers -t localhost/duranta-preview/postgis:golden tools/postgis
podman build --layers -t localhost/duranta-preview/smspit:golden tools/smspit
