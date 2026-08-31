#!/usr/bin/env node

import { randomUUID } from 'node:crypto';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createInterface } from 'node:readline/promises';
import { spawnSync } from 'node:child_process';
import {
  AwsCli,
  CliError,
  DEFAULTS,
  MARKER_PREFIX,
  NAMES,
  PARAMETERS,
  STANDARD_INSTANCE_QUOTA_CODE,
  assertManagedResource,
  buildAssumeRolePolicy,
  buildAwsArgs,
  buildBootstrapUserData,
  buildDnsUpsertChanges,
  buildGoldenAmiFilters,
  buildHostname,
  buildRoute53ChangeArgs,
  buildRunInstancesArgs,
  buildSendSshPublicKeyArgs,
  buildSetupRolePolicy,
  buildSshArgs,
  buildTags,
  canonicalDuration,
  commandExists,
  countManagedInstancesForCreator,
  creatorIdFromIdentity,
  expiresAt,
  getSsmParameter,
  isManagedResource,
  makeConfig,
  markerRecordName,
  normalizeDnsLabel,
  normalizeDomain,
  normalizeHostedZoneId,
  normalizeOwner,
  ownerCandidatesFromIdentity,
  parseDuration,
  parseRoute53TxtValue,
  publicKeyFromIdentity,
  tagsFromAws,
  validateCpuInstanceType,
  validateGoldenAmi,
} from './lib.mjs';

const HELP = `Duranta disposable preview environments

Usage:
  preview.mjs create <issue> [options]
  preview.mjs list [--json]
  preview.mjs show <name> [--json]
  preview.mjs connect <name> [--identity <key>] [--no-agent-forwarding]
  preview.mjs open <name>
  preview.mjs extend <name> <duration> [--identity <key>]
  preview.mjs terminate <name> [--yes]
  preview.mjs cleanup [--owner <owner>] [--yes]
  preview.mjs doctor [options]
  preview.mjs setup [options] [--apply]

Commands:
  create      Launch a preview, publish DNS, and wait until it is healthy
  list        List instances tagged ManagedBy=duranta-preview
  show        Show one managed preview by hostname, issue, or instance ID
  connect     Inject a laptop SSH key with EC2 Instance Connect, then SSH over SSM
  open        Open the public preview URL
  extend      Extend the on-instance termination deadline
  terminate   Terminate one managed instance and delete its exact DNS records
  cleanup     Delete stale owned DNS records under one owner's subdomain
  doctor      Run read-only local and AWS prerequisite checks
  setup       Plan minimal shared AWS prerequisites; --apply creates them

Common options:
  --profile <name>              AWS profile (default: preview)
  --region <region>             AWS region (default: us-west-2)
  --domain <domain>             Preview domain (default: duranta-preview.com)
  --owner <owner>               DNS owner label; otherwise inferred
  --hosted-zone-id <id>         Route53 public hosted zone
  --subnet-id <id>              Public subnet
  --security-group-id <id>      Web security group
  --instance-profile <name>     EC2 instance profile
  --ami-id <id>                 Golden preview AMI
  --help                        Show help

Create options:
  --instance-type <type>        Default: m7i.4xlarge
  --volume-size <GiB>           Default: 200
  --ttl <duration>              Default: 48h

Connect options:
  --no-agent-forwarding         Disable SSH agent forwarding

Configuration can also use DURANTA_PREVIEW_* environment variables and the
conventional SSM parameters under /duranta-preview/. Setup never mutates AWS
unless --apply is present.
`;

const BOOLEAN_OPTIONS = new Set(['help', 'json', 'yes', 'apply', 'no-agent-forwarding']);
const VALUE_OPTIONS = new Set([
  'profile', 'region', 'domain', 'owner', 'hosted-zone-id', 'subnet-id',
  'security-group-id', 'instance-profile', 'role-name', 'ami-id', 'vpc-id',
  'instance-type', 'volume-size', 'ttl', 'ssh-user', 'identity',
]);

function optionKey(value) {
  return value.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
}

export function parseCliArgs(argv) {
  const positionals = [];
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const token = argv[index];
    if (token === '-h') {
      options.help = true;
      continue;
    }
    if (!token.startsWith('--')) {
      positionals.push(token);
      continue;
    }
    const [rawName, inlineValue] = token.slice(2).split(/=(.*)/s, 2);
    if (BOOLEAN_OPTIONS.has(rawName)) {
      if (inlineValue !== undefined) throw new CliError(`--${rawName} does not accept a value`);
      options[optionKey(rawName)] = true;
      continue;
    }
    if (!VALUE_OPTIONS.has(rawName)) throw new CliError(`Unknown option: --${rawName}`);
    const value = inlineValue ?? argv[++index];
    if (value === undefined || value.startsWith('--')) throw new CliError(`--${rawName} requires a value`);
    options[optionKey(rawName)] = value;
  }
  return { positionals, options };
}

function envValue(name, env = process.env) {
  return env[`DURANTA_PREVIEW_${name}`];
}

function flattenInstances(response) {
  return (response.Reservations ?? []).flatMap((reservation) => reservation.Instances ?? []);
}

function stripDot(value) {
  const name = String(value ?? '').replace(/\.$/, '').toLowerCase();
  return name.startsWith(String.raw`\052.`) ? `*.${name.slice(5)}` : name;
}

function instanceState(instance) {
  return instance.State?.Name ?? 'unknown';
}

function activeInstance(instance) {
  return !['shutting-down', 'terminated'].includes(instanceState(instance));
}

function displayInstance(instance, domain = DEFAULTS.domain) {
  const tags = tagsFromAws(instance.Tags);
  const hostname = tags.Hostname || tags.Name || '';
  return {
    name: hostname,
    issue: tags.Issue || '',
    owner: tags.Owner || '',
    state: instanceState(instance),
    instanceId: instance.InstanceId,
    instanceType: instance.InstanceType,
    publicIp: instance.PublicIpAddress || '',
    expiresAt: tags.ExpiresAt || '',
    url: hostname ? `https://${hostname}/` : `https://${domain}/`,
  };
}

function printRows(rows) {
  if (!rows.length) {
    console.log('No managed preview instances found.');
    return;
  }
  const columns = ['name', 'state', 'instanceId', 'publicIp', 'expiresAt'];
  const widths = Object.fromEntries(columns.map((column) => [
    column,
    Math.max(column.length, ...rows.map((row) => String(row[column] ?? '').length)),
  ]));
  console.log(columns.map((column) => column.padEnd(widths[column])).join('  '));
  for (const row of rows) console.log(columns.map((column) => String(row[column] ?? '').padEnd(widths[column])).join('  '));
}

function getManagedInstances(aws, { includeTerminated = false } = {}) {
  const states = includeTerminated
    ? ['pending', 'running', 'stopping', 'stopped', 'shutting-down', 'terminated']
    : ['pending', 'running', 'stopping', 'stopped'];
  const response = aws.run([
    'ec2', 'describe-instances',
    '--filters', JSON.stringify([
      { Name: 'tag:ManagedBy', Values: [DEFAULTS.managedBy] },
      { Name: 'instance-state-name', Values: states },
    ]),
  ]);
  return flattenInstances(response).filter((instance) => isManagedResource(instance.Tags));
}

function resolveManagedInstance(aws, name) {
  const input = stripDot(name);
  const normalized = normalizeDnsLabel(name, 'name');
  const matches = getManagedInstances(aws).filter((instance) => {
    const tags = tagsFromAws(instance.Tags);
    return instance.InstanceId === name
      || stripDot(tags.Hostname) === input
      || stripDot(tags.Name) === input
      || tags.Issue === normalized;
  });
  if (!matches.length) throw new CliError(`Managed preview not found: ${name}`);
  if (matches.length > 1) {
    const names = matches.map((instance) => tagsFromAws(instance.Tags).Hostname).join(', ');
    throw new CliError(`More than one preview matches ${name}: ${names}. Use the full hostname or instance ID.`);
  }
  return assertManagedResource(matches[0], `instance ${matches[0].InstanceId}`);
}

function findTag(tags, key) {
  return tagsFromAws(tags)[key];
}

function validateHostedZone(aws, hostedZoneId, domain) {
  const id = normalizeHostedZoneId(hostedZoneId);
  const response = aws.run(['route53', 'get-hosted-zone', '--id', id]);
  const actual = stripDot(response.HostedZone?.Name);
  if (actual !== normalizeDomain(domain)) throw new CliError(`Hosted zone ${id} is for ${actual}, not ${domain}`);
  if (response.HostedZone?.Config?.PrivateZone) throw new CliError(`Hosted zone ${id} is private; a public hosted zone is required`);
  return { id, zone: response.HostedZone };
}

function configuredValue(aws, explicit, envName, parameter) {
  return explicit ?? envValue(envName) ?? getSsmParameter(aws, parameter);
}

function resolveHostedZone(aws, config, options = {}) {
  const configured = configuredValue(aws, options.hostedZoneId, 'HOSTED_ZONE_ID', PARAMETERS.hostedZoneId);
  if (configured) return validateHostedZone(aws, configured, config.domain);
  const response = aws.run([
    'route53', 'list-hosted-zones-by-name',
    '--dns-name', `${config.domain}.`,
    '--max-items', '10',
  ]);
  const match = (response.HostedZones ?? []).find((zone) => stripDot(zone.Name) === config.domain && !zone.Config?.PrivateZone);
  if (!match) throw new CliError(`Public Route53 hosted zone not found for ${config.domain}; pass --hosted-zone-id or run setup`);
  return validateHostedZone(aws, match.Id, config.domain);
}

function validateSubnet(aws, subnetId) {
  const response = aws.run(['ec2', 'describe-subnets', '--subnet-ids', subnetId]);
  const subnet = response.Subnets?.[0];
  if (!subnet || subnet.State !== 'available') throw new CliError(`Subnet is not available: ${subnetId}`);
  return subnet;
}

function defaultVpc(aws) {
  const response = aws.run([
    'ec2', 'describe-vpcs',
    '--filters', JSON.stringify([{ Name: 'is-default', Values: ['true'] }]),
  ]);
  return response.Vpcs?.[0] ?? null;
}

function resolveVpc(aws, options = {}) {
  const configured = configuredValue(aws, options.vpcId, 'VPC_ID', PARAMETERS.vpcId);
  if (configured) {
    const response = aws.run(['ec2', 'describe-vpcs', '--vpc-ids', configured]);
    if (!response.Vpcs?.[0]) throw new CliError(`VPC not found: ${configured}`);
    return response.Vpcs[0];
  }
  const vpc = defaultVpc(aws);
  if (!vpc) throw new CliError('No VPC configured and no default VPC exists; pass --vpc-id');
  return vpc;
}

function subnetHasInternetGateway(aws, subnet) {
  let tables = aws.run([
    'ec2', 'describe-route-tables',
    '--filters', JSON.stringify([{ Name: 'association.subnet-id', Values: [subnet.SubnetId] }]),
  ]).RouteTables ?? [];
  if (!tables.length) {
    tables = aws.run([
      'ec2', 'describe-route-tables',
      '--filters', JSON.stringify([
        { Name: 'vpc-id', Values: [subnet.VpcId] },
        { Name: 'association.main', Values: ['true'] },
      ]),
    ]).RouteTables ?? [];
  }
  return tables.some((table) => (table.Routes ?? []).some((route) => (
    route.DestinationCidrBlock === '0.0.0.0/0'
      && String(route.GatewayId ?? '').startsWith('igw-')
      && route.State === 'active'
  )));
}

function resolveSubnet(aws, options = {}) {
  const configured = configuredValue(aws, options.subnetId, 'SUBNET_ID', PARAMETERS.subnetId);
  if (configured) {
    const subnet = validateSubnet(aws, configured);
    if (!subnetHasInternetGateway(aws, subnet)) throw new CliError(`Subnet ${configured} has no active internet gateway route`);
    return subnet;
  }
  const tagged = aws.run([
    'ec2', 'describe-subnets',
    '--filters', JSON.stringify([
      { Name: 'tag:ManagedBy', Values: [DEFAULTS.managedBy] },
      { Name: 'tag:Role', Values: ['public'] },
      { Name: 'state', Values: ['available'] },
    ]),
  ]).Subnets ?? [];
  const candidates = tagged.length ? tagged : (() => {
    const vpc = resolveVpc(aws, options);
    return aws.run([
      'ec2', 'describe-subnets',
      '--filters', JSON.stringify([
        { Name: 'vpc-id', Values: [vpc.VpcId] },
        { Name: 'state', Values: ['available'] },
      ]),
    ]).Subnets ?? [];
  })();
  const publicSubnets = candidates.filter((subnet) => subnetHasInternetGateway(aws, subnet));
  publicSubnets.sort((left, right) => `${left.AvailabilityZone}:${left.SubnetId}`.localeCompare(`${right.AvailabilityZone}:${right.SubnetId}`));
  if (!publicSubnets.length) throw new CliError('No public subnet found; pass --subnet-id or run setup');
  return publicSubnets[0];
}

function validateSecurityGroup(aws, securityGroupId, vpcId) {
  const response = aws.run(['ec2', 'describe-security-groups', '--group-ids', securityGroupId]);
  const group = response.SecurityGroups?.[0];
  if (!group) throw new CliError(`Security group not found: ${securityGroupId}`);
  if (vpcId && group.VpcId !== vpcId) throw new CliError(`Security group ${securityGroupId} is not in VPC ${vpcId}`);
  return group;
}

function resolveSecurityGroup(aws, subnet, options = {}) {
  const configured = configuredValue(aws, options.securityGroupId, 'SECURITY_GROUP_ID', PARAMETERS.securityGroupId);
  if (configured) return validateSecurityGroup(aws, configured, subnet.VpcId);
  const response = aws.run([
    'ec2', 'describe-security-groups',
    '--filters', JSON.stringify([
      { Name: 'vpc-id', Values: [subnet.VpcId] },
      { Name: 'group-name', Values: [NAMES.securityGroup] },
    ]),
  ]);
  const group = response.SecurityGroups?.[0];
  if (!group) throw new CliError(`Security group ${NAMES.securityGroup} not found; pass --security-group-id or run setup --apply`);
  return group;
}

function validateInstanceProfile(aws, name) {
  const response = aws.run(['iam', 'get-instance-profile', '--instance-profile-name', name]);
  const profile = response.InstanceProfile;
  if (!profile) throw new CliError(`Instance profile not found: ${name}`);
  if (!(profile.Roles ?? []).length) throw new CliError(`Instance profile ${name} has no role attached`);
  return profile;
}

function resolveInstanceProfile(aws, options = {}) {
  const name = configuredValue(aws, options.instanceProfile, 'INSTANCE_PROFILE', PARAMETERS.instanceProfile) ?? NAMES.instanceProfile;
  return validateInstanceProfile(aws, name);
}

function callerIdentity(aws) {
  const identity = aws.run(['sts', 'get-caller-identity']);
  if (!identity?.Account) throw new CliError('AWS caller identity has no account ID');
  creatorIdFromIdentity(identity);
  return identity;
}

function validateAmi(aws, amiId, identity) {
  const response = aws.run(['ec2', 'describe-images', '--image-ids', amiId]);
  const image = response.Images?.[0];
  return validateGoldenAmi(image, identity.Account);
}

function resolveAmi(aws, options = {}, identity = callerIdentity(aws)) {
  const configured = configuredValue(aws, options.amiId, 'AMI_ID', PARAMETERS.amiId);
  if (configured) return validateAmi(aws, configured, identity);
  const response = aws.run([
    'ec2', 'describe-images', '--owners', 'self',
    '--filters', JSON.stringify(buildGoldenAmiFilters()),
  ]);
  const images = response.Images ?? [];
  images.sort((left, right) => String(right.CreationDate).localeCompare(String(left.CreationDate)));
  if (!images[0]) throw new CliError(`No golden AMI found; pass --ami-id or run the image bake command`);
  return validateAmi(aws, images[0].ImageId, identity);
}

function inferOwner(aws, options = {}, identity = null) {
  const configured = options.owner ?? envValue('OWNER');
  if (configured) return normalizeOwner(configured);
  const resolvedIdentity = identity ?? aws.attempt(['sts', 'get-caller-identity']).value;
  const identityOwner = ownerCandidatesFromIdentity(resolvedIdentity)[0];
  if (identityOwner) return identityOwner;
  for (const key of ['user.email', 'user.name']) {
    const result = spawnSync('git', ['config', '--get', key], { encoding: 'utf8', timeout: 10000 });
    if (result.status === 0 && result.stdout.trim()) return normalizeOwner(result.stdout.trim());
  }
  const github = spawnSync('gh', ['api', 'user', '--jq', '.login'], { encoding: 'utf8', timeout: 10000 });
  if (github.status === 0 && github.stdout.trim()) return normalizeOwner(github.stdout.trim());
  throw new CliError('Could not infer an owner; pass --owner or set DURANTA_PREVIEW_OWNER');
}

function inspectCpuInstanceType(aws, instanceType) {
  const response = aws.run(['ec2', 'describe-instance-types', '--instance-types', instanceType]);
  const details = response.InstanceTypes?.[0];
  return { details, vcpus: validateCpuInstanceType(instanceType, details) };
}

function standardQuota(aws) {
  return Number(aws.run([
    'service-quotas', 'get-service-quota',
    '--service-code', 'ec2',
    '--quota-code', STANDARD_INSTANCE_QUOTA_CODE,
  ]).Quota?.Value);
}

function checkStandardQuota(aws, instanceType) {
  const inspected = inspectCpuInstanceType(aws, instanceType);
  const limit = standardQuota(aws);
  if (!Number.isFinite(limit) || limit < inspected.vcpus) {
    throw new CliError(
      `EC2 On-Demand Standard quota is ${Number.isFinite(limit) ? limit : 'unknown'} vCPUs; ${instanceType} requires ${inspected.vcpus}. Request at least ${inspected.vcpus} vCPUs for quota ${STANDARD_INSTANCE_QUOTA_CODE}.`,
    );
  }
  return { ...inspected, quota: limit };
}

function describeInstance(aws, instanceId) {
  const response = aws.run(['ec2', 'describe-instances', '--instance-ids', instanceId]);
  const instance = flattenInstances(response)[0];
  if (!instance) throw new CliError(`Instance not found after launch: ${instanceId}`);
  return instance;
}

function sleep(milliseconds) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds);
}

function waitForSsm(aws, instanceId, timeoutMs = 600000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const response = aws.run([
      'ssm', 'describe-instance-information',
      '--filters', JSON.stringify([{ Key: 'InstanceIds', Values: [instanceId] }]),
    ]);
    const item = response.InstanceInformationList?.[0];
    if (item?.PingStatus === 'Online') return;
    sleep(10000);
  }
  throw new CliError(`SSM agent did not become online within ${Math.round(timeoutMs / 60000)} minutes`);
}

async function waitForHealthcheck(hostname, timeoutMs = 1200000) {
  const deadline = Date.now() + timeoutMs;
  let lastError = 'not attempted';
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`https://${hostname}/healthcheck`, {
        redirect: 'manual',
        signal: AbortSignal.timeout(10000),
      });
      if (response.ok) return;
      lastError = `HTTP ${response.status}`;
    } catch (error) {
      lastError = error.message;
    }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 10000));
  }
  throw new CliError(`https://${hostname}/healthcheck was not ready within ${Math.round(timeoutMs / 60000)} minutes (${lastError})`);
}

function assertRunning(instance) {
  if (instanceState(instance) !== 'running') {
    throw new CliError(`Instance ${instance.InstanceId} is ${instanceState(instance)}, not running`);
  }
}

function binaryInstalled(command) {
  const result = spawnSync(command, ['--version'], { encoding: 'utf8', timeout: 10000 });
  return !result.error || result.error.code !== 'ENOENT';
}

function injectSshKey(aws, config, instance, options) {
  assertRunning(instance);
  if (!binaryInstalled('session-manager-plugin')) {
    throw new CliError('session-manager-plugin is missing; install it before connecting: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html');
  }
  const key = publicKeyFromIdentity(options.identity);
  if (!/^ssh-(?:rsa|ed25519)|^ecdsa-sha2-/.test(key.publicKey)) {
    throw new CliError('The selected public key is not in OpenSSH format');
  }
  const response = aws.run(buildSendSshPublicKeyArgs(instance, config.sshUser, key.publicKey));
  if (!response.Success) throw new CliError(`EC2 Instance Connect rejected the SSH key for ${instance.InstanceId}`);
  return key;
}

function runSsh(aws, config, instance, options, remoteArgs = [], capture = false) {
  const key = injectSshKey(aws, config, instance, options);
  let temporaryDirectory;
  let identityFile = key.identityFile;
  if (!identityFile) {
    temporaryDirectory = mkdtempSync(join(tmpdir(), 'duranta-preview-ssh-'));
    identityFile = join(temporaryDirectory, 'selected-key.pub');
    writeFileSync(identityFile, `${key.publicKey}\n`, { mode: 0o600 });
  }
  try {
    const result = spawnSync('ssh', buildSshArgs(
      config,
      instance,
      identityFile,
      remoteArgs,
      { forwardAgent: !options.noAgentForwarding },
    ), capture
      ? { encoding: 'utf8', timeout: 120000 }
      : { stdio: 'inherit' });
    if (result.error) throw new CliError(`Unable to run ssh: ${result.error.message}`);
    if (result.status !== 0) {
      const details = capture ? String(result.stderr || result.stdout || '').trim() : '';
      throw new CliError(`SSH exited with code ${result.status}${details ? `\n${details}` : ''}`);
    }
    return capture ? String(result.stdout).trim() : '';
  } finally {
    if (temporaryDirectory) rmSync(temporaryDirectory, { recursive: true, force: true });
  }
}

async function confirm(message) {
  if (!process.stdin.isTTY) throw new CliError(`${message} Re-run with --yes.`);
  const prompt = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await prompt.question(`${message} [y/N] `);
    return /^y(?:es)?$/i.test(answer.trim());
  } finally {
    prompt.close();
  }
}

async function createPreview(aws, config, issue, options) {
  const identity = callerIdentity(aws);
  const creatorId = creatorIdFromIdentity(identity);
  const owner = inferOwner(aws, options, identity);
  const hostname = buildHostname(issue, owner, config.domain);
  const normalizedIssue = normalizeDnsLabel(issue, 'issue');
  parseDuration(config.ttl);
  const managedInstances = getManagedInstances(aws);
  const duplicate = managedInstances.find((instance) => stripDot(findTag(instance.Tags, 'Hostname')) === hostname);
  if (duplicate) throw new CliError(`Preview already exists: ${hostname} (${duplicate.InstanceId})`);
  const creatorCount = countManagedInstancesForCreator(
    managedInstances,
    creatorId,
    ownerCandidatesFromIdentity(identity),
  );
  if (creatorCount >= 10) {
    throw new CliError(`AWS caller ${creatorId} already has ${creatorCount} active managed instances; the limit is 10`);
  }

  checkStandardQuota(aws, config.instanceType);
  const hostedZone = resolveHostedZone(aws, config, options);
  assertDnsUnitAvailable(listZoneRecords(aws, hostedZone.id), hostname, owner);
  const subnet = resolveSubnet(aws, options);
  const securityGroup = resolveSecurityGroup(aws, subnet, options);
  const instanceProfile = resolveInstanceProfile(aws, options);
  const image = resolveAmi(aws, options, identity);
  const expiration = expiresAt(config.ttl);
  const tags = buildTags({ creatorId, issue: normalizedIssue, owner, hostname, expiresAt: expiration });
  const userData = buildBootstrapUserData({
    hostname,
    hostedZoneId: hostedZone.id,
    issue: normalizedIssue,
    owner,
    expiresAt: expiration,
  });
  const launch = aws.run(buildRunInstancesArgs({
    amiId: image.ImageId,
    clientToken: randomUUID(),
    instanceProfile: instanceProfile.InstanceProfileName,
    instanceType: config.instanceType,
    rootDeviceName: image.RootDeviceName,
    securityGroupId: securityGroup.GroupId,
    subnetId: subnet.SubnetId,
    tags,
    userData,
    volumeSize: config.volumeSize,
  }), { timeout: 180000 });
  const instanceId = launch.Instances?.[0]?.InstanceId;
  if (!instanceId) throw new CliError('EC2 did not return an instance ID');

  console.log(`Launched ${instanceId}; waiting for its public IPv4 address...`);
  try {
    aws.run(['ec2', 'wait', 'instance-running', '--instance-ids', instanceId], { timeout: 900000 });
    const instance = describeInstance(aws, instanceId);
    const publicIp = instance.PublicIpAddress;
    if (!publicIp) throw new CliError(`Instance ${instanceId} has no public IPv4 address`);
    const changes = buildDnsUpsertChanges({ action: 'CREATE', hostname, instanceId, owner, publicIp });
    const dns = aws.run(buildRoute53ChangeArgs(hostedZone.id, changes, `Create preview ${hostname}`));
    if (dns.ChangeInfo?.Id) {
      aws.run(['route53', 'wait', 'resource-record-sets-changed', '--id', dns.ChangeInfo.Id], { timeout: 300000 });
    }
    console.log(`DNS is live; waiting for EC2, SSM, and application health...`);
    aws.run(['ec2', 'wait', 'instance-status-ok', '--instance-ids', instanceId], { timeout: 900000 });
    waitForSsm(aws, instanceId);
    await waitForHealthcheck(hostname);
  } catch (error) {
    throw new CliError([
      error.message,
      `Instance ${instanceId} may still be running and accruing cost.`,
      'If bootstrap failed, user-data requested automatic instance termination; verify it anyway.',
      `Inspect: preview.mjs show ${instanceId}`,
      `Diagnose: preview.mjs connect ${instanceId}`,
      `Remove: preview.mjs terminate ${instanceId} --yes`,
    ].join('\n'));
  }

  console.log(`URL: https://${hostname}/`);
  console.log(`SSH: preview.mjs connect ${hostname}`);
  console.log(`Expires: ${expiration}`);
}

function listPreviews(aws, config, options) {
  const rows = getManagedInstances(aws).map((instance) => displayInstance(instance, config.domain));
  rows.sort((left, right) => left.name.localeCompare(right.name));
  if (options.json) console.log(JSON.stringify(rows, null, 2));
  else printRows(rows);
}

function showPreview(aws, config, name, options) {
  const row = displayInstance(resolveManagedInstance(aws, name), config.domain);
  if (options.json) console.log(JSON.stringify(row, null, 2));
  else for (const [key, value] of Object.entries(row)) console.log(`${key}: ${value}`);
}

function connectPreview(aws, config, name, options) {
  const instance = resolveManagedInstance(aws, name);
  runSsh(aws, config, instance, options);
}

function openPreview(aws, config, name) {
  const instance = resolveManagedInstance(aws, name);
  const hostname = findTag(instance.Tags, 'Hostname');
  if (!hostname) throw new CliError(`Instance ${instance.InstanceId} has no Hostname tag`);
  const url = `https://${hostname}/`;
  const command = process.platform === 'darwin' ? 'open' : 'xdg-open';
  const result = spawnSync(command, [url], { stdio: 'ignore' });
  if (result.error || result.status !== 0) console.log(url);
}

function extendPreview(aws, config, name, duration, options) {
  const instance = resolveManagedInstance(aws, name);
  const output = runSsh(
    aws,
    config,
    instance,
    options,
    ['sudo', '/usr/local/bin/duranta-preview-ttl', 'extend', canonicalDuration(duration)],
    true,
  );
  const matches = output.match(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g);
  const expiration = matches?.at(-1);
  if (!expiration || Number.isNaN(Date.parse(expiration))) {
    throw new CliError(`The remote TTL helper succeeded but did not print an ISO deadline:\n${output}`);
  }
  assertManagedResource(instance, `instance ${instance.InstanceId}`);
  aws.run([
    'ec2', 'create-tags',
    '--resources', instance.InstanceId,
    '--tags', JSON.stringify([{ Key: 'ExpiresAt', Value: expiration }]),
  ]);
  console.log(`Expires: ${expiration}`);
}

function listZoneRecords(aws, hostedZoneId) {
  return aws.run([
    'route53', 'list-resource-record-sets',
    '--hosted-zone-id', normalizeHostedZoneId(hostedZoneId),
  ]).ResourceRecordSets ?? [];
}

function exactRecord(records, name, type) {
  const normalized = stripDot(name);
  return records.find((record) => stripDot(record.Name) === normalized && record.Type === type) ?? null;
}

function parseMarkerRecord(record) {
  if (!record || record.Type !== 'TXT' || record.ResourceRecords?.length !== 1) return null;
  const encoded = parseRoute53TxtValue(record.ResourceRecords[0].Value);
  if (typeof encoded !== 'string') return null;
  try {
    return JSON.parse(encoded);
  } catch {
    return null;
  }
}

export function assertDnsUnitAvailable(records, hostname, owner) {
  const expected = new Set([
    stripDot(hostname),
    stripDot(`*.${hostname}`),
    stripDot(markerRecordName(hostname)),
  ]);
  const unit = records.filter((record) => expected.has(stripDot(record.Name)));
  if (!unit.length) return;

  const markerRecord = exactRecord(unit, markerRecordName(hostname), 'TXT');
  const marker = parseMarkerRecord(markerRecord);
  const app = exactRecord(unit, hostname, 'A');
  const wildcard = exactRecord(unit, `*.${hostname}`, 'A');
  let markerOwner;
  try {
    markerOwner = normalizeOwner(marker?.owner);
  } catch {}
  const oneAddress = (record) => record?.ResourceRecords?.length === 1
    && /^(?:\d{1,3}\.){3}\d{1,3}$/.test(record.ResourceRecords[0].Value);
  const managed = unit.length === 3
    && marker?.managedBy === DEFAULTS.managedBy
    && /^i-[a-z0-9]+$/i.test(String(marker.instanceId ?? ''))
    && markerOwner === normalizeOwner(owner)
    && oneAddress(app)
    && oneAddress(wildcard)
    && app.ResourceRecords[0].Value === wildcard.ResourceRecords[0].Value;
  if (managed) {
    throw new CliError(`Managed DNS already exists for ${hostname}; terminate or clean it up before creating a replacement`);
  }
  throw new CliError(`Refusing to clobber unmanaged or malformed DNS records for ${hostname}`);
}

export function ownedDnsRecords(records, instance) {
  const tags = tagsFromAws(instance.Tags);
  const hostname = stripDot(tags.Hostname);
  if (!hostname) return { records: [], warning: `Instance ${instance.InstanceId} has no Hostname tag` };
  const markerRecord = exactRecord(records, markerRecordName(hostname), 'TXT');
  const marker = parseMarkerRecord(markerRecord);
  let markerOwner;
  try {
    markerOwner = normalizeOwner(marker?.owner);
  } catch {}
  if (!marker
    || marker.managedBy !== DEFAULTS.managedBy
    || marker.instanceId !== instance.InstanceId
    || markerOwner !== tags.Owner) {
    return { records: [], warning: `DNS ownership marker is missing or invalid for ${hostname}` };
  }
  const app = exactRecord(records, hostname, 'A');
  const wildcard = exactRecord(records, `*.${hostname}`, 'A');
  if (!instance.PublicIpAddress) {
    return { records: [], warning: `Instance ${instance.InstanceId} has no public IP to verify DNS ownership` };
  }
  for (const record of [app, wildcard].filter(Boolean)) {
    const values = record.ResourceRecords?.map(({ Value }) => Value) ?? [];
    if (values.length !== 1 || values[0] !== instance.PublicIpAddress) {
      return { records: [], warning: `DNS ${record.Name} no longer points to ${instance.PublicIpAddress}` };
    }
  }
  return { records: [app, wildcard, markerRecord].filter(Boolean), warning: null };
}

export async function terminatePreview(aws, config, name, options) {
  const instance = resolveManagedInstance(aws, name);
  assertManagedResource(instance, `instance ${instance.InstanceId}`);
  const hostname = findTag(instance.Tags, 'Hostname');
  if (!options.yes && !await confirm(`Terminate ${hostname || instance.InstanceId} (${instance.InstanceId}) and clean up verified DNS?`)) {
    console.log('Cancelled.');
    return;
  }
  aws.run(['ec2', 'modify-instance-attribute', '--instance-id', instance.InstanceId, '--no-disable-api-stop']);
  aws.run(['ec2', 'terminate-instances', '--instance-ids', instance.InstanceId]);
  let deleted = 0;
  let warning;
  try {
    const hostedZone = resolveHostedZone(aws, config, options);
    const plan = ownedDnsRecords(listZoneRecords(aws, hostedZone.id), instance);
    warning = plan.warning;
    if (plan.records.length) {
      const changes = plan.records.map((record) => ({ Action: 'DELETE', ResourceRecordSet: record }));
      aws.run(buildRoute53ChangeArgs(hostedZone.id, changes, `Delete preview ${hostname}`));
      deleted = changes.length;
    }
  } catch (error) {
    warning = error.message;
  }
  console.log(`Terminating ${instance.InstanceId}; deleted ${deleted} DNS record sets.`);
  if (warning) console.warn(`DNS cleanup skipped: ${warning}`);
}

function markerHostname(record) {
  const name = stripDot(record.Name);
  return name.startsWith(MARKER_PREFIX) ? name.slice(MARKER_PREFIX.length) : null;
}

async function cleanupDns(aws, config, options) {
  const owner = inferOwner(aws, options);
  const hostedZone = resolveHostedZone(aws, config, options);
  const records = listZoneRecords(aws, hostedZone.id);
  const instances = new Map(getManagedInstances(aws, { includeTerminated: true }).map((instance) => [instance.InstanceId, instance]));
  const ownerSuffix = `.${owner}.${config.domain}`;
  const stale = [];

  for (const markerRecord of records.filter((record) => record.Type === 'TXT' && stripDot(record.Name).startsWith(MARKER_PREFIX))) {
    const hostname = markerHostname(markerRecord);
    if (!hostname?.endsWith(ownerSuffix)) continue;
    const marker = parseMarkerRecord(markerRecord);
    if (!marker || marker.managedBy !== DEFAULTS.managedBy) continue;
    let markerOwner;
    try {
      markerOwner = normalizeOwner(marker.owner);
    } catch {
      continue;
    }
    if (markerOwner !== owner) continue;
    const instance = instances.get(marker.instanceId);
    if (instance) {
      const tags = tagsFromAws(instance.Tags);
      if (!isManagedResource(instance.Tags) || tags.Owner !== owner || stripDot(tags.Hostname) !== hostname) continue;
      if (activeInstance(instance)) continue;
    }
    const unit = [
      exactRecord(records, hostname, 'A'),
      exactRecord(records, `*.${hostname}`, 'A'),
      markerRecord,
    ].filter(Boolean);
    stale.push({ hostname, records: unit });
  }

  if (!stale.length) {
    console.log(`No stale owned DNS records found under ${owner}.${config.domain}.`);
    return;
  }
  for (const item of stale) console.log(`${item.hostname}: ${item.records.length} record sets`);
  if (!options.yes && !await confirm(`Delete ${stale.length} stale preview DNS units?`)) {
    console.log('Cancelled.');
    return;
  }
  const changes = stale.flatMap((item) => item.records.map((record) => ({ Action: 'DELETE', ResourceRecordSet: record })));
  aws.run(buildRoute53ChangeArgs(hostedZone.id, changes, `Clean stale previews for ${owner}`));
  console.log(`Deleted ${changes.length} stale DNS record sets.`);
}

function missingAwsEntity(result) {
  const output = `${result.result?.stderr ?? ''}${result.result?.stdout ?? ''}`;
  return /NoSuchEntity|InvalidGroup\.NotFound|InvalidParameterValue/.test(output);
}

function getRoleIfPresent(aws, name) {
  const result = aws.attempt(['iam', 'get-role', '--role-name', name]);
  if (result.ok) return result.value.Role;
  if (missingAwsEntity(result)) return null;
  throw result.error;
}

function getInstanceProfileIfPresent(aws, name) {
  const result = aws.attempt(['iam', 'get-instance-profile', '--instance-profile-name', name]);
  if (result.ok) return result.value.InstanceProfile;
  if (missingAwsEntity(result)) return null;
  throw result.error;
}

export function assertSetupResourceOwned(resource, description) {
  if (resource && !isManagedResource(resource.Tags)) {
    throw new CliError(`Refusing to adopt or modify untagged ${description}`);
  }
  return resource;
}

function findSetupSecurityGroup(aws, vpcId, options) {
  const configured = options.securityGroupId ?? envValue('SECURITY_GROUP_ID') ?? getSsmParameter(aws, PARAMETERS.securityGroupId);
  if (configured) return validateSecurityGroup(aws, configured, vpcId);
  const response = aws.run([
    'ec2', 'describe-security-groups',
    '--filters', JSON.stringify([
      { Name: 'vpc-id', Values: [vpcId] },
      { Name: 'group-name', Values: [NAMES.securityGroup] },
    ]),
  ]);
  return response.SecurityGroups?.[0] ?? null;
}

function hasWorldIngress(group, port) {
  return (group?.IpPermissions ?? []).some((permission) => (
    permission.IpProtocol === 'tcp'
      && permission.FromPort <= port
      && permission.ToPort >= port
      && (permission.IpRanges ?? []).some((range) => range.CidrIp === '0.0.0.0/0')
  ));
}

function roleHasSsmPolicy(aws, roleName) {
  const response = aws.run(['iam', 'list-attached-role-policies', '--role-name', roleName]);
  return (response.AttachedPolicies ?? []).some((policy) => policy.PolicyArn === 'arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore');
}

function setupParameterValues(config, zone, vpc, subnet, groupId, profileName, roleName) {
  return {
    [PARAMETERS.domain]: config.domain,
    [PARAMETERS.hostedZoneId]: zone.id,
    [PARAMETERS.vpcId]: vpc.VpcId,
    [PARAMETERS.subnetId]: subnet.SubnetId,
    [PARAMETERS.securityGroupId]: groupId,
    [PARAMETERS.instanceProfile]: profileName,
    [PARAMETERS.roleName]: roleName,
  };
}

async function setup(aws, config, options) {
  const zone = resolveHostedZone(aws, config, options);
  const vpc = resolveVpc(aws, options);
  const subnet = resolveSubnet(aws, { ...options, vpcId: vpc.VpcId });
  if (subnet.VpcId !== vpc.VpcId) {
    throw new CliError(`Subnet ${subnet.SubnetId} is in ${subnet.VpcId}, not configured VPC ${vpc.VpcId}`);
  }
  const roleName = options.roleName ?? envValue('ROLE_NAME') ?? getSsmParameter(aws, PARAMETERS.roleName) ?? NAMES.role;
  const profileName = options.instanceProfile ?? envValue('INSTANCE_PROFILE') ?? getSsmParameter(aws, PARAMETERS.instanceProfile) ?? NAMES.instanceProfile;
  let group = findSetupSecurityGroup(aws, vpc.VpcId, options);
  let role = getRoleIfPresent(aws, roleName);
  let profile = getInstanceProfileIfPresent(aws, profileName);
  assertSetupResourceOwned(role, `IAM role ${roleName}`);
  assertSetupResourceOwned(profile, `instance profile ${profileName}`);
  const actions = [];
  const groupIsOurs = !group || isManagedResource(group.Tags);

  if (group && !groupIsOurs && (!hasWorldIngress(group, 80) || !hasWorldIngress(group, 443))) {
    throw new CliError(`Refusing to modify unmanaged security group ${group.GroupId}; use ${NAMES.securityGroup} or pass a web-ready group`);
  }

  if (!group) actions.push(`create security group ${NAMES.securityGroup} in ${vpc.VpcId}`);
  if (!group || !hasWorldIngress(group, 80)) actions.push('allow public TCP 80 on the preview web security group');
  if (!group || !hasWorldIngress(group, 443)) actions.push('allow public TCP 443 on the preview web security group');
  if (!role) actions.push(`create EC2 role ${roleName}`);
  if (!role || !roleHasSsmPolicy(aws, roleName)) actions.push(`attach AmazonSSMManagedInstanceCore to ${roleName}`);
  actions.push(`ensure Route53 A/TXT policy for ${config.domain} on ${roleName}`);
  if (!profile) actions.push(`create instance profile ${profileName}`);
  if (!profile || !(profile.Roles ?? []).some((item) => item.RoleName === roleName)) actions.push(`attach role ${roleName} to profile ${profileName}`);
  actions.push('write conventional /duranta-preview/* SSM parameters');

  console.log(options.apply ? 'Applying minimal Preview prerequisites:' : 'Preview prerequisite plan:');
  for (const action of actions) console.log(`- ${action}`);
  console.log(`- use public subnet ${subnet.SubnetId} (${subnet.AvailabilityZone})`);
  console.log('- no SSH ingress rule will be created');
  if (!options.apply) {
    console.log('\nNo changes made. Re-run with --apply to execute this plan.');
    return;
  }

  if (!group) {
    const response = aws.run([
      'ec2', 'create-security-group',
      '--group-name', NAMES.securityGroup,
      '--description', 'Public HTTP and HTTPS for disposable Duranta previews',
      '--vpc-id', vpc.VpcId,
      '--tag-specifications', JSON.stringify([{
        ResourceType: 'security-group',
        Tags: [
          { Key: 'Name', Value: NAMES.securityGroup },
          { Key: 'ManagedBy', Value: DEFAULTS.managedBy },
        ],
      }]),
    ]);
    group = validateSecurityGroup(aws, response.GroupId, vpc.VpcId);
  }

  const missingPorts = [80, 443].filter((port) => !hasWorldIngress(group, port));
  if (missingPorts.length) {
    aws.run([
      'ec2', 'authorize-security-group-ingress',
      '--group-id', group.GroupId,
      '--ip-permissions', JSON.stringify(missingPorts.map((port) => ({
        IpProtocol: 'tcp',
        FromPort: port,
        ToPort: port,
        IpRanges: [{ CidrIp: '0.0.0.0/0', Description: 'Public preview web traffic' }],
      }))),
    ]);
  }

  if (!role) {
    const response = aws.run([
      'iam', 'create-role',
      '--role-name', roleName,
      '--description', 'Minimal role for disposable Duranta preview instances',
      '--assume-role-policy-document', JSON.stringify(buildAssumeRolePolicy()),
      '--tags', JSON.stringify([{ Key: 'ManagedBy', Value: DEFAULTS.managedBy }]),
    ]);
    role = response.Role;
  }
  aws.run([
    'iam', 'attach-role-policy',
    '--role-name', roleName,
    '--policy-arn', 'arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore',
  ]);
  aws.run([
    'iam', 'put-role-policy',
    '--role-name', roleName,
    '--policy-name', NAMES.rolePolicy,
    '--policy-document', JSON.stringify(buildSetupRolePolicy(zone.id, config.domain)),
  ]);

  if (!profile) {
    const response = aws.run([
      'iam', 'create-instance-profile',
      '--instance-profile-name', profileName,
      '--tags', JSON.stringify([{ Key: 'ManagedBy', Value: DEFAULTS.managedBy }]),
    ]);
    profile = response.InstanceProfile;
  }
  const attachedRoles = profile.Roles ?? [];
  if (attachedRoles.length && !attachedRoles.some((item) => item.RoleName === roleName)) {
    throw new CliError(`Instance profile ${profileName} already contains another role; refusing to replace it`);
  }
  if (!attachedRoles.some((item) => item.RoleName === roleName)) {
    aws.run([
      'iam', 'add-role-to-instance-profile',
      '--instance-profile-name', profileName,
      '--role-name', roleName,
    ]);
  }

  const parameters = setupParameterValues(config, zone, vpc, subnet, group.GroupId, profileName, roleName);
  for (const [name, value] of Object.entries(parameters)) {
    aws.run([
      'ssm', 'put-parameter',
      '--name', name,
      '--type', 'String',
      '--value', value,
      '--overwrite',
    ]);
  }
  console.log('Setup complete. Run preview.mjs doctor; the golden AMI and Standard On-Demand quota are separate prerequisites.');
}

function simulationPrincipalArn(aws, identity) {
  const match = String(identity.Arn ?? '').match(/^arn:aws:sts::(\d+):assumed-role\/(.+)\/[^/]+$/);
  if (match) {
    const roleName = match[2].split('/').at(-1);
    const role = aws.attempt(['iam', 'get-role', '--role-name', roleName]);
    if (role.ok && role.value.Role?.Arn) return role.value.Role.Arn;
    return `arn:aws:iam::${match[1]}:role/${match[2]}`;
  }
  return identity.Arn ?? null;
}

async function doctor(aws, config, options) {
  const checks = [];
  const check = async (name, action, hint) => {
    try {
      const detail = await action();
      checks.push({ ok: true, name, detail: detail || '' });
      return detail;
    } catch (error) {
      checks.push({ ok: false, name, detail: error.message, hint });
      return null;
    }
  };

  await check('Node.js', () => process.version);
  await check('AWS CLI', () => {
    if (!commandExists('aws')) throw new CliError('aws was not found');
    return 'installed';
  }, 'Install AWS CLI v2.');
  await check('OpenSSH', () => {
    if (!binaryInstalled('ssh')) throw new CliError('ssh was not found');
    return 'installed';
  }, 'Install an OpenSSH client.');
  await check('Session Manager plugin', () => {
    if (!binaryInstalled('session-manager-plugin')) throw new CliError('session-manager-plugin was not found');
    return 'installed';
  }, 'Install it from https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html');
  await check('Laptop SSH key', () => {
    const key = publicKeyFromIdentity(options.identity);
    return key.identityFile ? key.identityFile : 'ssh-agent';
  }, 'Run ssh-add or pass --identity <key>.');

  const identity = await check('AWS SSO session', () => {
    const value = aws.run(['sts', 'get-caller-identity']);
    return value;
  }, `Run: aws sso login --profile ${config.profile}`);
  if (identity) checks.at(-1).detail = identity.Arn?.split('/').at(-1) || 'authenticated';

  let zone;
  await check('Public hosted zone', () => {
    zone = resolveHostedZone(aws, config, options);
    return zone.id;
  }, `Create or configure the public ${config.domain} hosted zone.`);
  let subnet;
  await check('Public subnet', () => {
    subnet = resolveSubnet(aws, options);
    return `${subnet.SubnetId} (${subnet.AvailabilityZone})`;
  }, 'Pass --subnet-id or run setup.');
  await check('Web security group', () => {
    if (!subnet) throw new CliError('public subnet is unresolved');
    const group = resolveSecurityGroup(aws, subnet, options);
    if (!hasWorldIngress(group, 80) || !hasWorldIngress(group, 443)) {
      throw new CliError(`${group.GroupId} does not allow public TCP 80 and 443`);
    }
    const sshOpen = (group.IpPermissions ?? []).some((permission) => permission.IpProtocol === 'tcp'
      && permission.FromPort <= 22 && permission.ToPort >= 22
      && (permission.IpRanges ?? []).some((range) => range.CidrIp === '0.0.0.0/0'));
    if (sshOpen) throw new CliError(`${group.GroupId} exposes SSH to the internet`);
    return group.GroupId;
  }, 'Run setup --apply; it creates only public HTTP/HTTPS ingress.');
  await check('EC2 instance profile', () => resolveInstanceProfile(aws, options).InstanceProfileName, 'Run setup --apply.');
  await check('Golden AMI', () => {
    if (!identity || typeof identity !== 'object') throw new CliError('AWS identity is unresolved');
    return resolveAmi(aws, options, identity).ImageId;
  }, 'Run the separate golden AMI bake command.');
  await check('CPU-only Standard instance quota', () => {
    const inspected = checkStandardQuota(aws, config.instanceType);
    return `${inspected.quota} vCPU limit; ${inspected.vcpus} required by ${config.instanceType}`;
  }, `Use an x86_64 CPU instance in a Standard family and request enough vCPUs in ${config.region} (quota ${STANDARD_INSTANCE_QUOTA_CODE}).`);
  await check('EC2 Instance Connect permission', () => {
    if (!identity || typeof identity !== 'object') throw new CliError('AWS identity is unresolved');
    const principal = simulationPrincipalArn(aws, identity);
    if (!principal) throw new CliError('could not derive the IAM principal ARN');
    const result = aws.run([
      'iam', 'simulate-principal-policy',
      '--policy-source-arn', principal,
      '--action-names', 'ec2-instance-connect:SendSSHPublicKey',
      '--resource-arns', '*',
    ]);
    const decision = result.EvaluationResults?.[0]?.EvalDecision;
    if (decision !== 'allowed') throw new CliError(`policy simulation returned ${decision ?? 'no result'}`);
    return 'allowed';
  }, 'Grant the Preview SSO role ec2-instance-connect:SendSSHPublicKey. Connect also verifies this at runtime.');

  for (const item of checks) {
    console.log(`${item.ok ? '[ok]' : '[missing]'} ${item.name}${item.detail ? `: ${item.detail}` : ''}`);
    if (!item.ok && item.hint) console.log(`          ${item.hint}`);
  }
  if (checks.some((item) => !item.ok)) throw new CliError('Doctor found missing prerequisites.', 2);
}

function requirePositionals(command, values, count, usage) {
  if (values.length !== count) throw new CliError(`Usage: preview.mjs ${usage ?? command}`);
}

export async function main(argv = process.argv.slice(2)) {
  const { positionals, options } = parseCliArgs(argv);
  const command = positionals.shift();
  if (options.help || !command) {
    console.log(HELP);
    return;
  }
  const config = makeConfig(options);
  const aws = new AwsCli(config);

  switch (command) {
    case 'create':
      requirePositionals(command, positionals, 1, 'create <issue> [options]');
      await createPreview(aws, config, positionals[0], options);
      break;
    case 'list':
      requirePositionals(command, positionals, 0);
      listPreviews(aws, config, options);
      break;
    case 'show':
      requirePositionals(command, positionals, 1, 'show <name> [--json]');
      showPreview(aws, config, positionals[0], options);
      break;
    case 'connect':
      requirePositionals(command, positionals, 1, 'connect <name> [--identity <key>] [--no-agent-forwarding]');
      connectPreview(aws, config, positionals[0], options);
      break;
    case 'open':
      requirePositionals(command, positionals, 1, 'open <name>');
      openPreview(aws, config, positionals[0]);
      break;
    case 'extend':
      requirePositionals(command, positionals, 2, 'extend <name> <duration> [--identity <key>]');
      extendPreview(aws, config, positionals[0], positionals[1], options);
      break;
    case 'terminate':
      requirePositionals(command, positionals, 1, 'terminate <name> [--yes]');
      await terminatePreview(aws, config, positionals[0], options);
      break;
    case 'cleanup':
      requirePositionals(command, positionals, 0, 'cleanup [--owner <owner>] [--yes]');
      await cleanupDns(aws, config, options);
      break;
    case 'doctor':
      requirePositionals(command, positionals, 0, 'doctor [options]');
      await doctor(aws, config, options);
      break;
    case 'setup':
      requirePositionals(command, positionals, 0, 'setup [options] [--apply]');
      await setup(aws, config, options);
      break;
    default:
      throw new CliError(`Unknown command: ${command}\n\n${HELP}`);
  }
}

const invokedPath = process.argv[1] ? resolve(process.argv[1]) : null;
if (invokedPath && invokedPath === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof CliError ? error.message : error.stack || error.message);
    process.exitCode = error.exitCode ?? 1;
  });
}
