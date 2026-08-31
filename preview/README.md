# Duranta Preview CLI

This dependency-free Node.js CLI manages disposable CPU development machines in the Preview AWS account. Defaults are AWS profile `preview`, Oregon (`us-west-2`), and `duranta-preview.com`.

## Prerequisites

- Node.js 20 or newer
- AWS CLI v2 with the `preview` SSO profile
- AWS Session Manager plugin
- OpenSSH and a GitHub-capable key loaded in `ssh-agent`

```sh
aws sso login --profile preview
ssh-add -L
./preview/preview.mjs doctor
```

The `m7i.4xlarge` default needs 16 Standard On-Demand vCPUs. Request at least 32 vCPUs for EC2 quota `L-1216C47A` so one preview and one AMI builder can run together. `setup` does not change quotas.

## One-time AWS setup

Review the plan, apply it, then run the checks again:

```sh
./preview/preview.mjs setup
./preview/preview.mjs setup --apply
./preview/preview.mjs doctor
```

Without `--apply`, `setup` is read-only. With it, the command creates the web security group, the EC2 role and instance profile for SSM and scoped Route53 updates, and SSM configuration parameters. It does not create a VPC, expose port 22, or bake an AMI.

## Golden AMI

Review and run the bake from a machine whose GitHub SSH key is loaded:

```sh
./preview/bake.mjs bake
./preview/bake.mjs bake --apply
./preview/bake.mjs list
```

The bake starts a temporary builder, checks out `main`, warms the stack, publishes the new AMI pointer, keeps the two newest managed AMIs, and terminates the builder. Rebuild it about weekly. List or run a separate prune plan with:

```sh
./preview/bake.mjs list --json
./preview/bake.mjs prune --keep 2
./preview/bake.mjs prune --keep 2 --apply
```

`bake` and `prune` are read-only plans without `--apply`.

## Daily use

```sh
./preview/preview.mjs create DUR-5542 --owner vitalii
./preview/preview.mjs list
./preview/preview.mjs show dur-5542.vitalii.duranta-preview.com
./preview/preview.mjs open dur-5542.vitalii.duranta-preview.com
./preview/preview.mjs connect dur-5542.vitalii.duranta-preview.com
```

After connecting, switch the warm checkout to the target ref and rebuild:

```sh
cd /opt/duranta-preview/app
git fetch origin
git checkout <branch-or-sha>
duranta-preview-stack rebuild
```

The default connection forwards the laptop SSH agent for private Git access. After fetching the required ref, disconnect and reconnect without forwarding before building or running PR code:

```sh
./preview/preview.mjs connect dur-5542.vitalii.duranta-preview.com --no-agent-forwarding
```

For a restart without rebuilding images:

```sh
sudo systemctl restart duranta-preview-stack
```

Inspect and extend the default 48-hour deadline, or terminate early:

```sh
./preview/preview.mjs extend dur-5542.vitalii.duranta-preview.com 12h
./preview/preview.mjs terminate dur-5542.vitalii.duranta-preview.com
./preview/preview.mjs cleanup --owner vitalii
```

`cleanup` removes verified stale DNS records; it does not terminate instances. Add `--yes` to non-interactive `terminate` or `cleanup` calls.

## Runtime and lifecycle

- Default host: `m7i.4xlarge`, 16 vCPU and 64 GiB RAM
- Root disk: encrypted 200 GiB gp3 with delete-on-termination
- URL: `<issue>.<owner>.duranta-preview.com`
- Lifetime: 48 hours, with at most 10 active previews per AWS caller identity
- CPU-only: CVML and AI-backed actions are unavailable in this MVP
- Rootless Podman: no Docker engine; Docker Compose v2 is used only as Podman's compose provider

There is no EC2 stop, snapshot, resume, or preserved development state. API stop protection prevents a stopped EBS volume from lingering; the managed `terminate` command removes that protection immediately before termination. Expiration and shutdown terminate the instance and delete its root EBS volume. Commit or push work before termination.

At prices checked on 2026-08-28, one default Oregon host is approximately **$20.01 per 24 hours** or **$40.01 for 48 hours**, including `m7i.4xlarge`, 200 GiB gp3, and one public IPv4 address. AWS pricing, data transfer, and DNS charges can change.

`connect` sends a public key through EC2 Instance Connect for a short-lived authorization and opens SSH through SSM. The private key stays on the laptop. SSH agent forwarding lets the remote checkout and AMI builder use the developer's GitHub access, but a compromised remote host could use the forwarded agent during that session. Use `--no-agent-forwarding` after private Git operations, disconnect when finished, and do not connect to an untrusted image.

Diagnostics URLs are `https://uptrace.<hostname>` and `https://mailpit.<hostname>`. Read their generated credentials after connecting:

```sh
sudo cat /var/lib/duranta-preview/diagnostics-credentials
duranta-preview-stack status
duranta-preview-stack logs
```

Run `./preview/preview.mjs --help` and `./preview/bake.mjs --help` for overrides. Configuration can use `DURANTA_PREVIEW_*` variables or SSM parameters under `/duranta-preview/`.

## Tests

```sh
node --test preview/preview.test.mjs preview/bake.test.mjs preview/remote/*.test.mjs
```
