# Preview host runtime

The golden AMI is built from a clean `main` checkout and warmed Podman images and volumes. Runtime hostnames and credentials are generated at boot. Certificates, machine identity, SSH host keys, AWS credentials, and GitHub credentials are excluded from the image.

## Work on a target ref

```sh
cd /opt/duranta-preview/app
git fetch origin
git checkout <branch-or-sha>
duranta-preview-stack rebuild
```

`rebuild` is the safe default after changing refs. To restart without rebuilding images:

```sh
sudo systemctl restart duranta-preview-stack
```

Useful runtime commands:

```sh
duranta-preview-stack status
duranta-preview-stack logs
duranta-preview-stack logs backend frontend
duranta-preview-stack down
duranta-preview-stack up
sudo /usr/local/bin/duranta-preview-ttl show
sudo /usr/local/bin/duranta-preview-ttl extend 12h
```

Diagnostics are protected with generated Basic Auth credentials:

```sh
sudo cat /var/lib/duranta-preview/diagnostics-credentials
```

- `https://uptrace.<preview-hostname>`
- `https://mailpit.<preview-hostname>`

The stack runs as `ubuntu` with rootless Podman. Docker Compose v2 is installed only as Podman's compose provider; there is no Docker engine. Caddy is the only process binding public ports, while application services bind to loopback.

The frontend Vite configuration and local Logto bootstrap use compatibility shims outside the app checkout. CVML is not built or started in the CPU-only MVP, so AI-backed actions are unavailable.

The on-host timer terminates the instance at its deadline. The root EBS volume is deleted, and there is no EC2 stop, snapshot, resume, or persisted development state. Push work before the deadline. SSH agent forwarding exposes the laptop agent to the host only while the connection is open; disconnect when finished.
