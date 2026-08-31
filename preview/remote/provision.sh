#!/usr/bin/env bash
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "Run as root" >&2; exit 1; }
[[ -n "${SSH_AUTH_SOCK:-}" && -S "${SSH_AUTH_SOCK:-}" ]] || {
  echo "SSH agent forwarding is required to clone the private repository" >&2
  exit 1
}

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y \
  caddy ca-certificates certbot curl dbus-user-session ec2-instance-connect git git-lfs \
  jq nodejs openssl podman rsync slirp4netns fuse-overlayfs sudo tar uidmap unzip

temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
if ! command -v aws >/dev/null; then
  aws_cli_version=2.36.33
  curl -fsSLo "$temporary/awscliv2.zip" \
    "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-${aws_cli_version}.zip"
  unzip -q "$temporary/awscliv2.zip" -d "$temporary"
  "$temporary/aws/install"
fi

compose_version=5.1.3
install -d -m 0755 /usr/local/lib/docker/cli-plugins
curl -fsSLo /usr/local/lib/docker/cli-plugins/docker-compose \
  "https://github.com/docker/compose/releases/download/v${compose_version}/docker-compose-linux-x86_64"
chmod 0755 /usr/local/lib/docker/cli-plugins/docker-compose
ln -sfn /usr/local/lib/docker/cli-plugins/docker-compose /usr/local/bin/docker-compose

task_version=3.45.5
curl -fsSLo "$temporary/task.tgz" \
  "https://github.com/go-task/task/releases/download/v${task_version}/task_linux_amd64.tar.gz"
tar -xzf "$temporary/task.tgz" -C /usr/local/bin task
chmod 0755 /usr/local/bin/task

[[ "$(id -u ubuntu)" == 1000 ]] || { echo "ubuntu must have UID 1000" >&2; exit 1; }
grep -q '^ubuntu:' /etc/subuid || usermod --add-subuids 100000-165535 ubuntu
grep -q '^ubuntu:' /etc/subgid || usermod --add-subgids 100000-165535 ubuntu
loginctl enable-linger ubuntu
systemctl start user@1000.service

run_as_preview() {
  sudo -u ubuntu env \
    HOME=/home/ubuntu \
    XDG_RUNTIME_DIR=/run/user/1000 \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus \
    "$@"
}

run_as_preview systemctl --user enable --now podman.socket
run_as_preview podman version
run_as_preview env PODMAN_COMPOSE_PROVIDER=/usr/local/bin/docker-compose podman compose version
caddy version
task --version
aws --version

install -d -m 0755 -o ubuntu -g ubuntu /opt/duranta-preview
if [[ ! -d /opt/duranta-preview/app/.git ]]; then
  sudo -u ubuntu env \
    SSH_AUTH_SOCK="$SSH_AUTH_SOCK" \
    GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new' \
    git clone --branch main git@github.com:GetDuranta/app.git /opt/duranta-preview/app
fi
sudo -u ubuntu env SSH_AUTH_SOCK="$SSH_AUTH_SOCK" git -C /opt/duranta-preview/app lfs pull

root_source="$(findmnt -n -o SOURCE --target /)"
app_source="$(findmnt -n -o SOURCE --target /opt/duranta-preview/app)"
storage_source="$(findmnt -n -o SOURCE --target /home/ubuntu)"
[[ "$root_source" == "$app_source" && "$root_source" == "$storage_source" ]] || {
  echo "The repository must live on the root EBS volume" >&2
  exit 1
}

rm -rf /opt/duranta-preview/logto-sandbox
cp -a /opt/duranta-preview/app/tools/logto-sandbox /opt/duranta-preview/logto-sandbox
node /tmp/duranta-preview-remote/patch-logto.mjs \
  /opt/duranta-preview/logto-sandbox

install -d -m 0755 /usr/local/lib/duranta-preview /etc/systemd/system/caddy.service.d
install -m 0644 /tmp/duranta-preview-remote/compose.preview.yml /usr/local/lib/duranta-preview/
install -m 0644 /tmp/duranta-preview-remote/vite.preview.mjs /usr/local/lib/duranta-preview/
install -m 0644 /tmp/duranta-preview-remote/Caddyfile /etc/caddy/Caddyfile
install -m 0644 /tmp/duranta-preview-remote/caddy.service.d.conf /etc/systemd/system/caddy.service.d/preview.conf
install -m 0644 /tmp/duranta-preview-remote/duranta-preview-stack.service /etc/systemd/system/
install -m 0644 /tmp/duranta-preview-remote/duranta-preview-expiry.service /etc/systemd/system/
install -m 0644 /tmp/duranta-preview-remote/duranta-preview-expiry.timer /etc/systemd/system/
install -m 0755 /tmp/duranta-preview-remote/bootstrap.sh /usr/local/bin/duranta-preview-bootstrap
install -m 0755 /tmp/duranta-preview-remote/ttl.mjs /usr/local/bin/duranta-preview-ttl
install -m 0755 /tmp/duranta-preview-remote/stack.sh /usr/local/bin/duranta-preview-stack
install -m 0755 /tmp/duranta-preview-remote/build-images.sh /usr/local/bin/duranta-preview-build-images
install -m 0755 /tmp/duranta-preview-remote/dns-cleanup.sh /usr/local/lib/duranta-preview/
install -m 0755 /tmp/duranta-preview-remote/expire.sh /usr/local/lib/duranta-preview/
systemctl daemon-reload
systemctl disable --now caddy duranta-preview-stack.service || true
systemctl stop duranta-preview-expiry.timer || true
systemctl enable duranta-preview-expiry.timer

for image in \
  docker.io/library/redis:7-alpine \
  docker.io/clickhouse/clickhouse-server:26.4.4-alpine \
  docker.io/rustfs/rustfs:1.0.0-beta.10 \
  docker.io/cyberaxduranta/duranta-logto-fork:1.41.0-duranta.6 \
  docker.io/axllent/mailpit:latest \
  docker.io/uptrace/uptrace:2.1.0-beta.7 \
  docker.io/otel/opentelemetry-collector-contrib:0.155.0
do
  run_as_preview podman pull "$image"
done

run_as_preview /usr/local/bin/duranta-preview-build-images
/usr/local/bin/duranta-preview-bootstrap \
  --hostname warm.invalid \
  --issue warm \
  --owner image \
  --expires-at 2099-01-01T00:00:00Z \
  --prepare-only
if ! run_as_preview /usr/local/bin/duranta-preview-stack up; then
  run_as_preview /usr/local/bin/duranta-preview-stack status >&2 || true
  run_as_preview /usr/local/bin/duranta-preview-stack logs db >&2 || true
  exit 1
fi
run_as_preview /usr/local/bin/duranta-preview-stack down
run_as_preview podman volume rm duranta-preview_blob_data

rm -f \
  /opt/duranta-preview/app/.env.local \
  /opt/duranta-preview/app/config/preview.yaml \
  /opt/duranta-preview/app/frontend/website/.env.live.local
rm -rf /opt/duranta-preview/runtime /var/lib/duranta-preview
install -d -m 0755 /opt/duranta-preview/runtime /var/lib/duranta-preview
rm -rf /var/lib/caddy/.local/share/caddy

if [[ -n "$(git -C /opt/duranta-preview/app status --porcelain --untracked-files=all)" ]]; then
  echo "Warming changed the golden source checkout" >&2
  git -C /opt/duranta-preview/app status --short >&2
  exit 1
fi
if [[ -n "$(run_as_preview podman ps -aq)" ]]; then
  echo "Containers remain after warm-up" >&2
  exit 1
fi

rm -rf /root/.aws /home/ubuntu/.aws
rm -f /root/.bash_history /home/ubuntu/.bash_history
cloud-init clean --logs --seed
rm -f /etc/ssh/ssh_host_*
rm -f /var/lib/dbus/machine-id
truncate -s 0 /etc/machine-id
sync
