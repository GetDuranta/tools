# Duranta Preview

This directory manages disposable development machines in the dedicated Preview AWS account. The AWS account, region, network, DNS zone, instance sizes, and AMI pointer are intentionally fixed in `config.mjs`.

Everything that runs on the machine itself lives in the `app` repository under `tools/preview/`; this CLI only creates, connects to, extends, and terminates EC2 instances and bakes the golden AMI.

## Prerequisites

- Node.js 20+
- Current AWS CLI v2 with the `preview` SSO profile; the local EC2 service model must include `VolumeInitializationRate`
- AWS Session Manager plugin
- OpenSSH and a GitHub-capable key in `ssh-agent`

```sh
aws sso login --profile preview
ssh-add -L
```

## Daily use

```sh
./preview/preview.mjs create DUR-5542 --owner vitalii
./preview/preview.mjs list
./preview/preview.mjs connect dur-5542.vitalii.duranta-preview.com
./preview/preview.mjs extend dur-5542.vitalii.duranta-preview.com 12h
./preview/preview.mjs terminate dur-5542.vitalii.duranta-preview.com --yes
```

`create` returns once `https://<hostname>/a/` answers. CVML keeps loading its models on the CPU for several more minutes; `docker compose ps` on the host shows when it is healthy.

SSH agent forwarding is off by default. Enable it only for private Git operations, then disconnect before running code from the fetched ref:

```sh
./preview/preview.mjs connect dur-5542.vitalii.duranta-preview.com --forward-agent
```

On the host:

```sh
cd /opt/duranta-preview/app
git fetch origin
git checkout --detach <sha>
git lfs pull
```

Reconnect without `--forward-agent`, then rebuild and inspect the stack. The login environment (`/etc/profile.d/duranta-preview.sh`) already selects the Compose project, files, and profiles, so plain `docker compose` commands address the preview stack:

```sh
docker compose up -d --build
docker compose ps
docker compose logs -f backend frontend
```

The stack is the regular `compose.yml` from `app` with the `tools/preview/compose.yml` overlay: Traefik terminates TLS with a Let's Encrypt certificate, the backend proxies `/a/` to a compiled frontend bundle without source maps, Logto runs locally, and CVML runs on the CPU from the stock `cvml` service.

Mailpit and Smspit are available at `https://mailpit.<hostname>` and `https://smspit.<hostname>` behind Basic Auth. The generated credentials are on the host:

```sh
cat /opt/duranta-preview/diagnostics-credentials
```

## Lifecycle

- Default workspace: ARM64 `t4g.2xlarge`, 100 GiB encrypted gp3 root disk
- Workspace root volumes initialize from the golden snapshot at 300 MiB/s; AWS charges for provisioned initialization per snapshot GiB
- Default lifetime: 48 hours; maximum 10 active machines per AWS caller
- Root disk is deleted when the instance terminates
- EC2 stop is disabled; the lifetime is a scheduled OS `shutdown`, and the instance is configured to terminate on shutdown. `extend` reschedules that shutdown and updates the `ExpiresAt` tag
- User data schedules a 90-minute safety shutdown before bootstrap starts; bootstrap replaces it with the requested lifetime once the stack is up, and a failed bootstrap shuts the machine down immediately
- Expiration may leave two stale DNS records; a later create replaces them, while explicit terminate deletes the exact records best-effort
- Every taggable per-run resource carries `CreatorId`, human-readable `CreatedBy`, ISO `CreatedAt`, `ManagedBy`, and `Purpose`; workspace disks and ENIs also carry the hostname and expiry

There is no stop, resume, snapshot, or persistent development state. Push work before termination.
Route53 record sets and automatically assigned public IPv4 addresses do not support tags; shared subnet, security group, IAM, hosted zone, and SSM pointer resources are not recreated per workspace.

## Golden AMI

With explicit authorization for the temporary builder cost:

```sh
./preview/bake.mjs bake
```

The command starts one ARM64 `t4g.2xlarge` builder with a six-hour deadman, clones `main` of `app` with its Git LFS model artifacts, runs `tools/preview/provision.sh` from that checkout (Docker Engine, image builds, a warm-up run of the stack, cleanup of data volumes and host identity), publishes the architecture-specific AMI pointer, keeps the two newest ARM64 managed AMIs, and terminates the builder in `finally`. Run it about weekly or after changing the host tooling in `app`.

An AMI stores the checkout, the built images, and the build caches, not a running stack. Every new workspace starts the stack and loads the CVML models again.

AMIs baked before the move of the host tooling into `app` do not contain `tools/preview/bootstrap.sh`; `create` from such a pointer fails: user data cannot find the script and shuts the machine down. Bake once after merging the `app` side before using this CLI. `create` never falls back to an image of another architecture.

T4g instances run in Unlimited mode so fresh builders and workspaces can burst immediately. Sustained average CPU utilization above the 40% baseline can incur surplus CPU credit charges.

## Tests

```sh
node --test preview/*.test.mjs
```
