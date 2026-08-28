#!/usr/bin/env bash
set -euo pipefail

deadline_file="${DURANTA_PREVIEW_DEADLINE:-/var/lib/duranta-preview/deadline}"
if [[ -f "$deadline_file" ]]; then
  if ! deadline="$(date -u -d "$(<"$deadline_file")" +%s 2>/dev/null)"; then
    deadline="$(date -u -d "$(uptime -s) + 60 minutes" +%s)"
  fi
else
  deadline="$(date -u -d "$(uptime -s) + 60 minutes" +%s)"
fi
if (( $(date -u +%s) < deadline )); then
  exit 0
fi

/usr/local/lib/duranta-preview/dns-cleanup.sh || true
shutdown -h now
