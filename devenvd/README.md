# EC2 development environments

The control plane is one Lambda container image with two handlers:

- `bootstrap.api` serves the IAM-authenticated HTTP API.
- `bootstrap.reconcile` executes lifecycle operations, lease events, and periodic cleanup.

Build it with `make devenvd-image`. The gateway has a separate image built with
`make devenv-gateway-image`.

## CLI

Set `DURANTA_DEV_ENV_API_URL` to the API Gateway URL, then run:

```sh
ddev env create my-task
ddev env list
ddev env open <environment-id>
ddev env ssh <environment-id>
ddev env extend <environment-id> --hours 4
ddev env stop <environment-id>
ddev env archive <environment-id> --checkpoint-name vitalii-my-task
ddev env checkpoint list
ddev env checkpoint delete <checkpoint-id>
ddev env delete <environment-id>
```

Environment commands default to AWS profile `be-dev` and region `us-west-2`.
`create` uploads the current commit plus modified tracked files. It refuses an
untracked working tree unless `--include-untracked` is explicitly supplied.
The `gpu-cvml` profile additionally includes hydrated Git LFS files; the
standard profile keeps LFS pointers and uses shared CVML.

## Required configuration

Both handlers use `STATE_TABLE_NAME`, `OWNER_INDEX_NAME`, `DUE_INDEX_NAME`,
`OWNER_NAMESPACE`, `SOURCE_STAGING_BUCKET`, `RECONCILER_FUNCTION_ARN`,
`LEASE_TARGET_ARN`, `SCHEDULER_ROLE_ARN`, `SCHEDULER_GROUP_NAME`, and
`LIFECYCLE_DLQ_ARN`.

The API handler also uses `GATEWAY_ROLE_ARN`, `INTERACTIVE_ROLE_ARN_PREFIXES`,
`ALB_SIGNER_ARN`, and `ALB_CLIENT_ID`. `ALB_TRUST_EMAIL_CLAIM` defaults to
`false`. Keep it fail-closed when the IdP supplies a mapped verified-email
claim; enable trust only for a reviewed corporate SAML mapping.

The reconciler additionally uses `CPU_LAUNCH_TEMPLATE_ID`,
`GPU_LAUNCH_TEMPLATE_ID`, `PRIVATE_SUBNET_IDS`, `WORKSPACE_KMS_KEY_ARN`,
`WORKSPACE_SECURITY_GROUP_ID`, `INSTANCE_ROLE_ARN`, `PREVIEW_BASE_DOMAIN`, `SHARED_CVML_ENDPOINT`,
`LOGTO_ADMIN_BASE_URL`, `LOGTO_M2M_CLIENT_ID_PARAMETER`, and
`LOGTO_M2M_CLIENT_SECRET_PARAMETER`.

Preview Logto applications use the exact host with `/a/callback` as the
redirect URI and `/a/signin` as the post-logout redirect URI.

The workspace receives source through a 15-minute presigned download URL. It
does not need S3 or SSM Parameter Store permissions. Docker data, the checkout,
runtime certificates, and Compose volumes live on the persistent workspace
volume mounted at `/workspace`. A bounded extractor validates the source tar
before applying its worktree overlay as root.
