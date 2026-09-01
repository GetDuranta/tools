# Duranta Preview

This directory manages disposable development machines in the dedicated Preview AWS account. The AWS account, region, network, DNS zone, instance sizes, and AMI pointer are intentionally fixed in `config.mjs`.

## Prerequisites

- Node.js 20+
- AWS CLI v2 with the `preview` SSO profile
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

Reconnect without `--forward-agent`, then rebuild and inspect the rootless Podman stack:

```sh
duranta-preview-stack rebuild
duranta-preview-stack status
duranta-preview-stack logs backend frontend
```

The production frontend is the default. `duranta-preview-stack frontend-dev` enables Vite/HMR; run `duranta-preview-stack frontend-production` before sharing the URL.

Diagnostics are available at `https://uptrace.<hostname>` and `https://mailpit.<hostname>`. Their generated Basic Auth credentials are on the host:

```sh
sudo cat /var/lib/duranta-preview/diagnostics-credentials
```

## Lifecycle

- Default workspace: ARM64 `t4g.2xlarge`, 100 GiB encrypted gp3 root disk
- Default lifetime: 48 hours; maximum 10 active machines per AWS caller
- Root disk is deleted when the instance terminates
- EC2 stop is disabled; expiration performs an OS shutdown configured to terminate
- Bootstrap starts with a safety deadline of at most one hour and installs the requested deadline after the stack is healthy
- Expiration may leave two stale DNS records; a later create replaces them, while explicit terminate deletes the exact records best-effort
- Every taggable per-run resource carries `CreatorId`, human-readable `CreatedBy`, ISO `CreatedAt`, `ManagedBy`, and `Purpose`; workspace disks and ENIs also carry the hostname and expiry
- CVML is not included in this CPU-only version

There is no stop, resume, snapshot, or persistent development state. Push work before termination.
Route53 record sets and automatically assigned public IPv4 addresses do not support tags; shared subnet, security group, IAM, hosted zone, and SSM pointer resources are not recreated per workspace.

## Golden AMI

With explicit authorization for the temporary builder cost:

```sh
./preview/bake.mjs bake
```

The command starts one ARM64 `t4g.2xlarge` builder with a six-hour deadman, checks out and warms `main`, publishes the architecture-specific AMI pointer, keeps the two newest ARM64 managed AMIs, and terminates the builder in `finally`. The legacy x86 pointer and images remain available for rollback. Run it about weekly or after changing Preview host tooling.

For an architecture change, bake the new architecture-specific pointer and launch a fresh smoke-test workspace from the same checkout before merging or distributing the CLI change. Until the first ARM64 bake succeeds, `create` fails closed because the ARM64 pointer does not exist; it must never fall back to an incompatible x86 image.

T4g instances run in Unlimited mode so fresh builders and workspaces can burst immediately. Sustained average CPU utilization above the 40% baseline can incur surplus CPU credit charges.

## Tests

```sh
node --test preview/*.test.mjs preview/remote/*.test.mjs
```
