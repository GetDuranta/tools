#!/usr/bin/env bash
set -euo pipefail

hostname=""
issue=""
owner=""
hosted_zone_id=""
expires_at=""
prepare_only=false

while (($#)); do
  case "$1" in
    --hostname) hostname="${2:-}"; shift 2 ;;
    --issue) issue="${2:-}"; shift 2 ;;
    --owner) owner="${2:-}"; shift 2 ;;
    --hosted-zone-id) hosted_zone_id="${2:-}"; shift 2 ;;
    --expires-at) expires_at="${2:-}"; shift 2 ;;
    --prepare-only) prepare_only=true; shift ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ "$hostname" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || { echo "Invalid hostname" >&2; exit 2; }
[[ "$hostname" != *..* && ${#hostname} -le 253 ]] || { echo "Invalid hostname" >&2; exit 2; }
[[ "$issue" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "Invalid issue" >&2; exit 2; }
[[ "$owner" =~ ^[a-z0-9][a-z0-9-]*$ ]] || { echo "Invalid owner" >&2; exit 2; }
if [[ "$prepare_only" == false ]]; then
  [[ "$hosted_zone_id" =~ ^[A-Z0-9]+$ ]] || { echo "Invalid hosted zone ID" >&2; exit 2; }
fi
now_epoch="$(date -u +%s)"
deadline_epoch="$(date -u -d "$expires_at" +%s)" || { echo "Invalid expiration" >&2; exit 2; }
((deadline_epoch > now_epoch)) || { echo "Expiration must be in the future" >&2; exit 2; }
expires_at="$(date -u -d "@$deadline_epoch" +%Y-%m-%dT%H:%M:%SZ)"
safety_deadline_epoch="$deadline_epoch"
if [[ "$prepare_only" == false ]] && ((safety_deadline_epoch > now_epoch + 3600)); then
  safety_deadline_epoch=$((now_epoch + 3600))
fi
safety_expires_at="$(date -u -d "@$safety_deadline_epoch" +%Y-%m-%dT%H:%M:%SZ)"

runtime_dir=/opt/duranta-preview/runtime
app_dir=/opt/duranta-preview/app
state_dir=/var/lib/duranta-preview
certificate_root=/var/lib/caddy/preview-tls
certificate_name=duranta-preview
certificate_config_dir=$certificate_root/config
certificate_work_dir=$certificate_root/work
certificate_logs_dir=$certificate_root/logs
certificate_live_dir=$certificate_config_dir/live/$certificate_name
install -d -m 0755 "$runtime_dir" "$state_dir"
printf '%s\n' "$safety_expires_at" >"$state_dir/deadline"
chmod 0600 "$state_dir/deadline"
if [[ "$prepare_only" == false ]]; then
  systemctl enable --now duranta-preview-expiry.timer
fi

rustfs_access_key="preview$(openssl rand -hex 8)"
rustfs_secret_key="$(openssl rand -hex 32)"
logto_m2m_secret="$(openssl rand -hex 32)"
internal_api_key="$(openssl rand -base64 32)"
session_cookie_key="$(openssl rand -base64 32)"

cat >"$runtime_dir/compose.env" <<EOF
COMPOSE_PROJECT_NAME=duranta-preview
DURANTA_APP_DIR=$app_dir
DURANTA_RUNTIME_DIR=$runtime_dir
PREVIEW_HOSTNAME=$hostname
PREVIEW_FRONTEND_MODE=production
RUSTFS_ACCESS_KEY=$rustfs_access_key
RUSTFS_SECRET_KEY=$rustfs_secret_key
LOGTO_M2M_APP_SECRET=$logto_m2m_secret
EOF

if [[ "$prepare_only" == true ]]; then
  backend_logto_endpoint=http://logto:3001
  s3_endpoint=http://blobs:9000
else
  backend_logto_endpoint=https://logto.$hostname
  s3_endpoint=https://s3.$hostname
fi

cat >"$runtime_dir/preview.yaml" <<EOF
Web:
  PublicUrl: https://$hostname/a/
  ServerName: $hostname
  ExternallyVisibleServerName: $hostname
  CDNUrls:
    - https://$hostname
Auth:
  LogtoJwksUrl: $backend_logto_endpoint/oidc/jwks
  LogtoM2M:
    Endpoint: $backend_logto_endpoint
    M2MAppId: duranta-m2m-local
    M2MAppSecret: $logto_m2m_secret
  SessionCookieKey: $session_cookie_key
  InternalApiKey: $internal_api_key
Mixpanel:
  Token: ''
Secrets:
  OpenRouterApiKey: ''
O11y:
  Sentry:
    Dsn: ''
    Debug: false
Data:
  S3:
    CustomSettings: true
    Endpoint: $s3_endpoint
    AccessKeyId: $rustfs_access_key
    SecretAccessKey: $rustfs_secret_key
  BlobsBucket: duranta-blobs-preview
  TileCacheBucket: duranta-tile-cache-preview
  SessionRecCH: ''
Services:
  Cvml:
    HttpEndpoint: http://127.0.0.1:1
  Sendgrid:
    Enabled: false
    ApiKey: ''
Repos:
  Cvml:
    BlobBucket: duranta-cvml-preview
EOF

cat >"$app_dir/.env.local" <<EOF
BACKEND_EXTRA_ARGS=--configs=local,with-logto,preview
EOF
cat >"$app_dir/frontend/website/.env.live.local" <<EOF
VITE_PUBLIC_API_URL=https://$hostname
VITE_LOGTO_ENDPOINT=https://logto.$hostname
VITE_LOGTO_APP_ID=duranta-web-local
VITE_LOGTO_API_RESOURCE=https://api.getduranta.com
VITE_MIXPANEL_TOKEN=
VITE_UPTRACE_LOGS_ENDPOINT=
VITE_UPTRACE_TRACES_ENDPOINT=
VITE_UPTRACE_DSN=
VITE_SENTRY_DISABLED=true
EOF

install -m 0644 /usr/local/lib/duranta-preview/vite.preview.mjs "$runtime_dir/vite.preview.mjs"
sed "s#https://uptrace.local.getduranta.com#https://uptrace.$hostname#g" \
  "$app_dir/tools/uptrace/uptrace.yml" >"$runtime_dir/uptrace.yml"

diagnostics_password="$(openssl rand -hex 18)"
diagnostics_hash="$(caddy hash-password --plaintext "$diagnostics_password")"
printf 'preview:%s\n' "$diagnostics_password" >"$state_dir/diagnostics-credentials"
chmod 0600 "$state_dir/diagnostics-credentials"
cat >"$runtime_dir/caddy.env" <<EOF
PREVIEW_HOSTNAME=$hostname
DIAGNOSTICS_PASSWORD_HASH=$diagnostics_hash
PREVIEW_CERTIFICATE=$certificate_live_dir/fullchain.pem
PREVIEW_PRIVATE_KEY=$certificate_live_dir/privkey.pem
EOF
chmod 0600 "$runtime_dir/caddy.env"
chown ubuntu:ubuntu \
  "$runtime_dir/compose.env" \
  "$runtime_dir/preview.yaml" \
  "$runtime_dir/uptrace.yml" \
  "$runtime_dir/vite.preview.mjs" \
  "$app_dir/.env.local" \
  "$app_dir/frontend/website/.env.live.local"
chmod 0600 "$runtime_dir/compose.env" "$runtime_dir/preview.yaml"

if [[ "$prepare_only" == true ]]; then
  exit 0
fi

token="$(curl -fsS -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)"
metadata=(-fsS -H "X-aws-ec2-metadata-token: $token")
instance_id="$(curl "${metadata[@]}" http://169.254.169.254/latest/meta-data/instance-id)"
public_ip="$(curl "${metadata[@]}" http://169.254.169.254/latest/meta-data/public-ipv4)"
cat >"$state_dir/instance.env" <<EOF
PREVIEW_HOSTNAME=$hostname
ISSUE=$issue
OWNER=$owner
HOSTED_ZONE_ID=$hosted_zone_id
INSTANCE_ID=$instance_id
PUBLIC_IP=$public_ip
EOF
chmod 0600 "$state_dir/instance.env"

dns_ready=false
for _ in {1..120}; do
  app_ips="$(getent ahostsv4 "$hostname" 2>/dev/null | awk '{print $1}' | sort -u || true)"
  logto_ips="$(getent ahostsv4 "logto.$hostname" 2>/dev/null | awk '{print $1}' | sort -u || true)"
  if grep -Fxq "$public_ip" <<<"$app_ips" && grep -Fxq "$public_ip" <<<"$logto_ips"; then
    dns_ready=true
    break
  fi
  sleep 5
done
[[ "$dns_ready" == true ]] || {
  echo "DNS did not resolve $hostname and logto.$hostname to $public_ip" >&2
  exit 1
}

systemctl stop caddy
install -d -m 0700 "$certificate_root"
certbot certonly \
  --non-interactive \
  --agree-tos \
  --register-unsafely-without-email \
  --standalone \
  --preferred-challenges http \
  --config-dir "$certificate_config_dir" \
  --work-dir "$certificate_work_dir" \
  --logs-dir "$certificate_logs_dir" \
  --cert-name "$certificate_name" \
  --keep-until-expiring \
  --domain "$hostname" \
  --domain "logto.$hostname" \
  --domain "s3.$hostname" \
  --domain "mailpit.$hostname" \
  --domain "uptrace.$hostname"
chown -R caddy:caddy "$certificate_root"

systemctl enable caddy
systemctl reload-or-restart caddy
systemctl enable duranta-preview-stack.service
systemctl restart duranta-preview-stack.service
printf '%s\n' "$expires_at" >"$state_dir/deadline"
chmod 0600 "$state_dir/deadline"

echo "Preview: https://$hostname/a/"
echo "Diagnostics credentials: sudo cat $state_dir/diagnostics-credentials"
echo "Expires: $expires_at"
