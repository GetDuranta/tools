import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

export const DEFAULTS = Object.freeze({
  profile: 'preview',
  region: 'us-west-2',
  instanceType: 'm7i.4xlarge',
  volumeSize: 200,
  ttl: '48h',
  domain: 'duranta-preview.com',
  sshUser: 'ec2-user',
  managedBy: 'duranta-preview',
});

export const PARAMETERS = Object.freeze({
  amiId: '/duranta-preview/golden-ami-id',
  domain: '/duranta-preview/domain',
  hostedZoneId: '/duranta-preview/hosted-zone-id',
  instanceProfile: '/duranta-preview/instance-profile',
  roleName: '/duranta-preview/role-name',
  securityGroupId: '/duranta-preview/security-group-id',
  subnetId: '/duranta-preview/subnet-id',
  vpcId: '/duranta-preview/vpc-id',
});

export const NAMES = Object.freeze({
  instanceProfile: 'duranta-preview-instance',
  role: 'duranta-preview-instance',
  rolePolicy: 'DurantaPreviewDns',
  securityGroup: 'duranta-preview-web',
});

export const STANDARD_INSTANCE_QUOTA_CODE = 'L-1216C47A';
export const MARKER_PREFIX = '_duranta-preview.';

export class CliError extends Error {
  constructor(message, exitCode = 1) {
    super(message);
    this.name = 'CliError';
    this.exitCode = exitCode;
  }
}

function shortenLabel(value) {
  if (value.length <= 63) return value;
  const hash = createHash('sha256').update(value).digest('hex').slice(0, 8);
  return `${value.slice(0, 54).replace(/-+$/, '')}-${hash}`;
}

export function normalizeDnsLabel(value, label = 'value') {
  const normalized = String(value ?? '')
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  if (!normalized) throw new CliError(`${label} must contain at least one letter or digit`);
  return shortenLabel(normalized);
}

export function normalizeOwner(value) {
  const owner = String(value ?? '').trim();
  return normalizeDnsLabel(owner.includes('@') ? owner.split('@')[0] : owner, 'owner');
}

export function normalizeDomain(value) {
  const domain = String(value ?? '').trim().toLowerCase().replace(/\.$/, '');
  if (!/^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(domain)) {
    throw new CliError(`Invalid domain: ${value}`);
  }
  return domain;
}

export function buildHostname(issue, owner, domain = DEFAULTS.domain) {
  const hostname = `${normalizeDnsLabel(issue, 'issue')}.${normalizeOwner(owner)}.${normalizeDomain(domain)}`;
  if (hostname.length > 253) throw new CliError('Generated hostname exceeds the DNS length limit');
  return hostname;
}

export function parseDuration(value) {
  const input = String(value ?? '').trim().toLowerCase();
  if (!input) throw new CliError('Duration is required, for example 12h or 2d');
  const units = { s: 1, m: 60, h: 3600, d: 86400, w: 604800 };
  const pattern = /(\d+)([smhdw])/gy;
  let offset = 0;
  let seconds = 0;
  let match;
  while ((match = pattern.exec(input))) {
    if (match.index !== offset) throw new CliError(`Invalid duration: ${value}`);
    seconds += Number(match[1]) * units[match[2]];
    offset = pattern.lastIndex;
  }
  if (offset !== input.length || seconds <= 0 || !Number.isSafeInteger(seconds)) {
    throw new CliError(`Invalid duration: ${value}`);
  }
  return seconds;
}

export function canonicalDuration(value) {
  return `${parseDuration(value)}s`;
}

export function expiresAt(duration, now = new Date()) {
  return new Date(now.getTime() + parseDuration(duration) * 1000).toISOString();
}

export function buildTags({ creatorId, issue, owner, hostname, expiresAt: expiration, name = hostname }) {
  const tags = {
    Name: name,
    ManagedBy: DEFAULTS.managedBy,
    Issue: normalizeDnsLabel(issue, 'issue'),
    Owner: normalizeOwner(owner),
    Hostname: hostname,
    ExpiresAt: expiration,
  };
  if (creatorId) tags.CreatorId = creatorId;
  return tags;
}

export function tagsToAws(tags) {
  return Object.entries(tags).map(([Key, Value]) => ({ Key, Value: String(Value) }));
}

export function tagsFromAws(tags = []) {
  if (!Array.isArray(tags)) return { ...tags };
  return Object.fromEntries(tags.map(({ Key, Value }) => [Key, Value]));
}

export function isManagedResource(tags) {
  return tagsFromAws(tags).ManagedBy === DEFAULTS.managedBy;
}

export function assertManagedResource(resource, description = 'resource') {
  if (!resource || !isManagedResource(resource.Tags ?? resource.tags)) {
    throw new CliError(`Refusing to modify unmanaged ${description}`);
  }
  return resource;
}

export function usesStandardOnDemandQuota(instanceType) {
  return /^(?:[acdhimrtz]\d|i[ms]\d)/i.test(String(instanceType));
}

export function creatorIdFromIdentity(identity) {
  const creatorId = String(identity?.UserId ?? '').trim();
  if (!creatorId || creatorId.length > 256) throw new CliError('AWS caller identity has no usable UserId');
  return creatorId;
}

export function ownerCandidatesFromIdentity(identity) {
  const candidates = [];
  const arnSession = String(identity?.Arn ?? '').split('/').at(-1);
  const userSession = String(identity?.UserId ?? '').split(':').slice(1).join(':');
  for (const value of [arnSession, userSession]) {
    if (!value) continue;
    try {
      candidates.push(normalizeOwner(decodeURIComponent(value)));
    } catch {}
  }
  return [...new Set(candidates)];
}

export function countManagedInstancesForCreator(instances, creatorId, legacyOwners = []) {
  const owners = new Set(legacyOwners.map((owner) => normalizeOwner(owner)));
  return instances.filter((instance) => {
    if (!isManagedResource(instance.Tags ?? instance.tags)) return false;
    const tags = tagsFromAws(instance.Tags ?? instance.tags);
    if (tags.CreatorId) return tags.CreatorId === creatorId;
    return owners.has(tags.Owner);
  }).length;
}

export function validateCpuInstanceType(instanceType, details) {
  if (!details || details.InstanceType !== instanceType) {
    throw new CliError(`Could not inspect EC2 instance type ${instanceType}`);
  }
  const accelerator = [
    ['GPU', details.GpuInfo],
    ['FPGA', details.FpgaInfo],
    ['inference accelerator', details.InferenceAcceleratorInfo],
  ].find(([, info]) => info);
  if (accelerator) throw new CliError(`${instanceType} has a ${accelerator[0]}; Preview MVP is CPU-only`);
  if (!usesStandardOnDemandQuota(instanceType)) {
    throw new CliError(`${instanceType} is not in the supported EC2 Standard On-Demand quota families`);
  }
  if (!(details.ProcessorInfo?.SupportedArchitectures ?? []).includes('x86_64')) {
    throw new CliError(`${instanceType} does not support the x86_64 golden AMI`);
  }
  const vcpus = details.VCpuInfo?.DefaultVCpus;
  if (!Number.isInteger(vcpus) || vcpus <= 0) {
    throw new CliError(`Could not determine vCPU count for ${instanceType}`);
  }
  return vcpus;
}

export function validateGoldenAmi(image, accountId) {
  const amiId = image?.ImageId ?? 'configured AMI';
  if (!image || image.State !== 'available') throw new CliError(`Golden AMI is not available: ${amiId}`);
  if (!accountId || image.OwnerId !== accountId) throw new CliError(`Golden AMI ${amiId} is not owned by the current AWS account`);
  const tags = tagsFromAws(image.Tags);
  if (tags.ManagedBy !== DEFAULTS.managedBy || tags.Purpose !== 'golden') {
    throw new CliError(`AMI ${amiId} is not tagged as a Duranta Preview golden image`);
  }
  if (image.Architecture !== 'x86_64') throw new CliError(`Golden AMI ${amiId} must use x86_64`);
  if (image.RootDeviceType !== 'ebs' || !image.RootDeviceName) {
    throw new CliError(`Golden AMI ${amiId} must have an EBS root device`);
  }
  const ebsMappings = (image.BlockDeviceMappings ?? []).filter((mapping) => mapping.Ebs);
  const root = ebsMappings.find((mapping) => mapping.DeviceName === image.RootDeviceName);
  if (!root || root.Ebs.DeleteOnTermination !== true) {
    throw new CliError(`Golden AMI ${amiId} must have a disposable EBS root device`);
  }
  if (ebsMappings.some((mapping) => mapping.DeviceName !== image.RootDeviceName)) {
    throw new CliError(`Golden AMI ${amiId} contains an extra EBS device`);
  }
  return image;
}

export function buildAwsArgs(config, args, { json = true } = {}) {
  const base = ['--profile', config.profile, '--region', config.region, '--no-cli-pager'];
  if (json) base.push('--output', 'json');
  return [...base, ...args];
}

function commandError(command, args, result) {
  const details = String(result.stderr || result.stdout || '').trim();
  const suffix = details ? `\n${details}` : '';
  return new CliError(`${command} ${args.join(' ')} failed with exit code ${result.status ?? 'unknown'}${suffix}`);
}

export class AwsCli {
  constructor(config, runner = spawnSync) {
    this.config = config;
    this.runner = runner;
  }

  attempt(args, options = {}) {
    const awsArgs = buildAwsArgs(this.config, args, options);
    const result = this.runner('aws', awsArgs, {
      encoding: 'utf8',
      maxBuffer: 20 * 1024 * 1024,
      timeout: options.timeout ?? 120000,
    });
    if (result.error) return { ok: false, error: new CliError(`Unable to run aws: ${result.error.message}`), result };
    if (result.status !== 0) return { ok: false, error: commandError('aws', awsArgs, result), result };
    if (options.json === false || !String(result.stdout).trim()) return { ok: true, value: String(result.stdout).trim(), result };
    try {
      return { ok: true, value: JSON.parse(result.stdout), result };
    } catch (error) {
      return { ok: false, error: new CliError(`AWS CLI returned invalid JSON: ${error.message}`), result };
    }
  }

  run(args, options = {}) {
    const result = this.attempt(args, options);
    if (!result.ok) throw result.error;
    return result.value;
  }
}

export function makeConfig(options = {}, env = process.env) {
  const number = (value, name) => {
    const parsed = Number(value);
    if (!Number.isInteger(parsed) || parsed <= 0) throw new CliError(`${name} must be a positive integer`);
    return parsed;
  };
  return {
    profile: options.profile ?? env.DURANTA_PREVIEW_PROFILE ?? DEFAULTS.profile,
    region: options.region ?? env.DURANTA_PREVIEW_REGION ?? DEFAULTS.region,
    domain: normalizeDomain(options.domain ?? env.DURANTA_PREVIEW_DOMAIN ?? DEFAULTS.domain),
    instanceType: options.instanceType ?? env.DURANTA_PREVIEW_INSTANCE_TYPE ?? DEFAULTS.instanceType,
    volumeSize: number(options.volumeSize ?? env.DURANTA_PREVIEW_VOLUME_SIZE ?? DEFAULTS.volumeSize, 'volume size'),
    ttl: options.ttl ?? env.DURANTA_PREVIEW_TTL ?? DEFAULTS.ttl,
    sshUser: options.sshUser ?? env.DURANTA_PREVIEW_SSH_USER ?? DEFAULTS.sshUser,
  };
}

export function getSsmParameter(aws, name) {
  const result = aws.attempt(['ssm', 'get-parameter', '--name', name]);
  if (result.ok) return result.value.Parameter?.Value ?? null;
  const output = `${result.result?.stderr ?? ''}${result.result?.stdout ?? ''}`;
  if (output.includes('ParameterNotFound')) return null;
  throw result.error;
}

export function normalizeHostedZoneId(value) {
  return String(value ?? '').replace(/^\/hostedzone\//, '');
}

export function buildRunInstancesArgs({
  amiId,
  clientToken,
  instanceProfile,
  instanceType,
  rootDeviceName,
  securityGroupId,
  subnetId,
  tags,
  userData,
  volumeSize,
}) {
  const instanceTags = tagsToAws(tags);
  const volumeTags = tagsToAws({
    Name: `${tags.Name}-root`,
    ManagedBy: tags.ManagedBy,
    Issue: tags.Issue,
    Owner: tags.Owner,
    Hostname: tags.Hostname,
    ...(tags.CreatorId ? { CreatorId: tags.CreatorId } : {}),
  });
  return [
    'ec2', 'run-instances',
    '--image-id', amiId,
    '--instance-type', instanceType,
    '--min-count', '1',
    '--max-count', '1',
    '--client-token', clientToken,
    '--network-interfaces', JSON.stringify([{
      AssociatePublicIpAddress: true,
      DeleteOnTermination: true,
      DeviceIndex: 0,
      Groups: [securityGroupId],
      SubnetId: subnetId,
    }]),
    '--iam-instance-profile', JSON.stringify({ Name: instanceProfile }),
    '--metadata-options', JSON.stringify({
      HttpEndpoint: 'enabled',
      HttpTokens: 'required',
      HttpPutResponseHopLimit: 1,
      InstanceMetadataTags: 'enabled',
    }),
    '--block-device-mappings', JSON.stringify([{
      DeviceName: rootDeviceName,
      Ebs: {
        DeleteOnTermination: true,
        Encrypted: true,
        VolumeSize: volumeSize,
        VolumeType: 'gp3',
      },
    }]),
    '--instance-initiated-shutdown-behavior', 'terminate',
    '--disable-api-stop',
    '--tag-specifications',
    JSON.stringify([
      { ResourceType: 'instance', Tags: instanceTags },
      { ResourceType: 'volume', Tags: volumeTags },
    ]),
    '--user-data', userData,
  ];
}

export function buildGoldenAmiFilters() {
  return [
    { Name: 'state', Values: ['available'] },
    { Name: 'tag:ManagedBy', Values: [DEFAULTS.managedBy] },
    { Name: 'tag:Purpose', Values: ['golden'] },
  ];
}

export function buildBootstrapUserData({ hostname, hostedZoneId, issue, owner, expiresAt: expiration }) {
  return [
    '#!/bin/sh',
    'set -u',
    '/usr/local/bin/duranta-preview-bootstrap \\',
    `  --hostname ${hostname} \\`,
    `  --issue ${normalizeDnsLabel(issue, 'issue')} \\`,
    `  --owner ${normalizeOwner(owner)} \\`,
    `  --hosted-zone-id ${normalizeHostedZoneId(hostedZoneId)} \\`,
    `  --expires-at ${expiration}`,
    'status=$?',
    'if [ "$status" -eq 0 ]; then',
    '  exit 0',
    'fi',
    'message="duranta-preview bootstrap failed with status $status; requesting instance termination"',
    'printf "%s\\n" "$message" >&2',
    'if command -v logger >/dev/null 2>&1; then',
    '  logger -t duranta-preview "$message"',
    'fi',
    'sync',
    'if command -v shutdown >/dev/null 2>&1; then',
    '  shutdown -h now',
    'else',
    '  systemctl poweroff --no-block',
    'fi',
    'exit "$status"',
    '',
  ].join('\n');
}

export function buildDnsMarker({ instanceId, owner }) {
  return JSON.stringify({ managedBy: DEFAULTS.managedBy, instanceId, owner: normalizeOwner(owner) });
}

export function markerRecordName(hostname) {
  return `${MARKER_PREFIX}${hostname}`;
}

export function route53TxtValue(value) {
  return JSON.stringify(value);
}

export function parseRoute53TxtValue(value) {
  try {
    return JSON.parse(value);
  } catch {
    return null;
  }
}

export function buildDnsUpsertChanges({ action = 'UPSERT', hostname, instanceId, owner, publicIp }) {
  if (!['CREATE', 'UPSERT'].includes(action)) throw new CliError(`Unsupported DNS change action: ${action}`);
  const marker = buildDnsMarker({ instanceId, owner });
  return [
    {
      Action: action,
      ResourceRecordSet: {
        Name: hostname,
        Type: 'A',
        TTL: 60,
        ResourceRecords: [{ Value: publicIp }],
      },
    },
    {
      Action: action,
      ResourceRecordSet: {
        Name: `*.${hostname}`,
        Type: 'A',
        TTL: 60,
        ResourceRecords: [{ Value: publicIp }],
      },
    },
    {
      Action: action,
      ResourceRecordSet: {
        Name: markerRecordName(hostname),
        Type: 'TXT',
        TTL: 60,
        ResourceRecords: [{ Value: route53TxtValue(marker) }],
      },
    },
  ];
}

export function buildRoute53ChangeArgs(hostedZoneId, changes, comment) {
  return [
    'route53', 'change-resource-record-sets',
    '--hosted-zone-id', normalizeHostedZoneId(hostedZoneId),
    '--change-batch', JSON.stringify({ Comment: comment, Changes: changes }),
  ];
}

export function buildSsmProxyCommand(config) {
  const safe = /^[a-zA-Z0-9_+=,.@-]+$/;
  if (!safe.test(config.profile) || !safe.test(config.region)) {
    throw new CliError('Profile and region contain characters that are unsafe for OpenSSH ProxyCommand');
  }
  return `aws --profile ${config.profile} --region ${config.region} ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p`;
}

export function publicKeyFromIdentity(identity, runner = spawnSync) {
  if (identity) {
    if (!existsSync(identity)) throw new CliError(`Identity file not found: ${identity}`);
    if (identity.endsWith('.pub')) {
      const privatePath = identity.slice(0, -4);
      return {
        publicKey: readFileSync(identity, 'utf8').trim(),
        identityFile: existsSync(privatePath) ? privatePath : null,
      };
    }
    const publicPath = `${identity}.pub`;
    if (existsSync(publicPath)) {
      return { publicKey: readFileSync(publicPath, 'utf8').trim(), identityFile: identity };
    }
    const derived = runner('ssh-keygen', ['-y', '-f', identity], { encoding: 'utf8', timeout: 10000 });
    if (derived.status !== 0 || !String(derived.stdout).trim()) {
      throw new CliError(`Could not derive a public key from ${identity}; provide its .pub file or load it with ssh-add`);
    }
    return { publicKey: derived.stdout.trim(), identityFile: identity };
  }

  const result = runner('ssh-add', ['-L'], { encoding: 'utf8', timeout: 10000 });
  if (result.status !== 0) throw new CliError('No SSH key is available; run ssh-add or pass --identity <key>');
  const publicKey = String(result.stdout).split('\n').map((line) => line.trim()).find((line) => line && !line.startsWith('The agent'));
  if (!publicKey) throw new CliError('ssh-agent has no public keys; run ssh-add or pass --identity <key>');
  return { publicKey, identityFile: null };
}

export function buildSendSshPublicKeyArgs(instance, sshUser, publicKey) {
  return [
    'ec2-instance-connect', 'send-ssh-public-key',
    '--instance-id', instance.InstanceId,
    '--availability-zone', instance.Placement.AvailabilityZone,
    '--instance-os-user', sshUser,
    '--ssh-public-key', publicKey,
  ];
}

export function buildSshArgs(config, instance, identityFile, remoteArgs = []) {
  const args = [
    '-A',
    '-o', `ProxyCommand=${buildSsmProxyCommand(config)}`,
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', 'ConnectTimeout=20',
  ];
  if (identityFile) args.push('-o', 'IdentitiesOnly=yes', '-i', identityFile);
  args.push(`${config.sshUser}@${instance.InstanceId}`, ...remoteArgs);
  return args;
}

export function buildSetupRolePolicy(hostedZoneId, domain) {
  return {
    Version: '2012-10-17',
    Statement: [
      {
        Effect: 'Allow',
        Action: 'route53:ChangeResourceRecordSets',
        Resource: `arn:aws:route53:::hostedzone/${normalizeHostedZoneId(hostedZoneId)}`,
        Condition: {
          'ForAllValues:StringLike': {
            'route53:ChangeResourceRecordSetsNormalizedRecordNames': [`*.${normalizeDomain(domain)}`],
          },
          'ForAllValues:StringEquals': {
            'route53:ChangeResourceRecordSetsRecordTypes': ['A', 'TXT'],
          },
        },
      },
      {
        Effect: 'Allow',
        Action: 'route53:ListResourceRecordSets',
        Resource: `arn:aws:route53:::hostedzone/${normalizeHostedZoneId(hostedZoneId)}`,
      },
      {
        Effect: 'Allow',
        Action: 'route53:GetChange',
        Resource: 'arn:aws:route53:::change/*',
      },
    ],
  };
}

export function buildAssumeRolePolicy() {
  return {
    Version: '2012-10-17',
    Statement: [{
      Effect: 'Allow',
      Principal: { Service: 'ec2.amazonaws.com' },
      Action: 'sts:AssumeRole',
    }],
  };
}

export function commandExists(command, args = ['--version'], runner = spawnSync) {
  const result = runner(command, args, { encoding: 'utf8', timeout: 10000 });
  return !result.error && result.status === 0;
}
