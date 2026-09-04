import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

import { CONFIG } from './config.mjs';

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

export function buildHostname(issue, owner) {
  return `${normalizeDnsLabel(issue, 'issue')}.${normalizeOwner(owner)}.${CONFIG.domain}`;
}

export function parseDuration(value) {
  const input = String(value ?? '').trim().toLowerCase();
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
  if (!input || offset !== input.length || seconds <= 0 || !Number.isSafeInteger(seconds)) {
    throw new CliError(`Invalid duration: ${value}`);
  }
  return seconds;
}

export function expiresAt(duration, now = new Date()) {
  return new Date(now.getTime() + parseDuration(duration) * 1000).toISOString();
}

export function extendExpiration(current, duration, now = new Date()) {
  const currentDate = new Date(current);
  const base = Number.isFinite(currentDate.getTime())
    ? Math.max(currentDate.getTime(), now.getTime())
    : now.getTime();
  return new Date(base + parseDuration(duration) * 1000).toISOString();
}

export function buildTags({ auditTags, issue, owner, hostname, expiration }) {
  return {
    Name: hostname,
    ManagedBy: CONFIG.managedBy,
    Purpose: 'workspace',
    ...auditTags,
    Issue: normalizeDnsLabel(issue, 'issue'),
    Owner: normalizeOwner(owner),
    Hostname: hostname,
    ExpiresAt: expiration,
  };
}

export function tagsToAws(tags) {
  return Object.entries(tags).map(([Key, Value]) => ({ Key, Value: String(Value) }));
}

export function tagsFromAws(tags = []) {
  if (!Array.isArray(tags)) return { ...tags };
  return Object.fromEntries(tags.map(({ Key, Value }) => [Key, Value]));
}

export function isManagedResource(tags) {
  return tagsFromAws(tags).ManagedBy === CONFIG.managedBy;
}

export function assertManagedResource(resource, description = 'resource') {
  if (!resource || !isManagedResource(resource.Tags ?? resource.tags)) {
    throw new CliError(`Refusing to modify unmanaged ${description}`);
  }
  return resource;
}

export function creatorIdFromIdentity(identity) {
  const creatorId = String(identity?.UserId ?? '').trim();
  if (!creatorId) throw new CliError('AWS caller identity has no usable UserId');
  return creatorId;
}

export function buildAuditTags(identity, createdAt = new Date()) {
  const creatorId = creatorIdFromIdentity(identity);
  const arnSession = String(identity?.Arn ?? '').split('/').at(-1)?.trim();
  const separator = creatorId.indexOf(':');
  const userIdSession = separator === -1 ? creatorId : creatorId.slice(separator + 1);
  const encodedCreatedBy = arnSession || userIdSession;
  let createdBy = encodedCreatedBy;
  try {
    createdBy = decodeURIComponent(encodedCreatedBy);
  } catch {}

  const date = createdAt instanceof Date ? createdAt : new Date(createdAt);
  if (!Number.isFinite(date.getTime())) throw new CliError(`Invalid creation time: ${createdAt}`);
  return { CreatorId: creatorId, CreatedBy: createdBy, CreatedAt: date.toISOString() };
}

export function assertPreviewAccount(identity) {
  if (identity?.Account !== CONFIG.accountId) {
    throw new CliError(`AWS profile ${CONFIG.profile} is account ${identity?.Account ?? 'unknown'}, expected Preview account ${CONFIG.accountId}`);
  }
  creatorIdFromIdentity(identity);
  return identity;
}

export function countManagedInstancesForCreator(instances, creatorId) {
  return instances.filter((instance) => {
    const tags = tagsFromAws(instance.Tags ?? instance.tags);
    return isManagedResource(instance.Tags ?? instance.tags) && tags.CreatorId === creatorId;
  }).length;
}

export function buildAwsArgs(args, { json = true } = {}) {
  const base = ['--profile', CONFIG.profile, '--region', CONFIG.region, '--no-cli-pager'];
  if (json) base.push('--output', 'json');
  return [...base, ...args];
}

export function buildCreditSpecificationArgs(instanceType) {
  return /^t\d/.test(instanceType)
    ? ['--credit-specification', 'CpuCredits=unlimited']
    : [];
}

export function assertVolumeInitializationRateSupport(skeleton) {
  const ebs = skeleton?.BlockDeviceMappings?.[0]?.Ebs;
  if (!ebs || !Object.hasOwn(ebs, 'VolumeInitializationRate')) {
    throw new CliError('This Preview CLI requires an AWS CLI v2 EC2 model with VolumeInitializationRate support; update AWS CLI v2');
  }
  return skeleton;
}

function commandError(args, result) {
  const detail = String(result.stderr || result.stdout || '').trim();
  return new CliError(`aws ${args.join(' ')} failed${detail ? `\n${detail}` : ''}`);
}

export class AwsCli {
  constructor(runner = spawnSync) {
    this.runner = runner;
  }

  run(args, options = {}) {
    const awsArgs = buildAwsArgs(args, options);
    const result = this.runner('aws', awsArgs, {
      encoding: 'utf8',
      maxBuffer: 20 * 1024 * 1024,
      timeout: options.timeout ?? 120000,
    });
    if (result.error) throw new CliError(`Unable to run aws: ${result.error.message}`);
    if (result.status !== 0) throw commandError(awsArgs, result);
    if (options.json === false || !String(result.stdout).trim()) return String(result.stdout).trim();
    try {
      return JSON.parse(result.stdout);
    } catch (error) {
      throw new CliError(`AWS CLI returned invalid JSON: ${error.message}`);
    }
  }
}

export function validateGoldenAmi(image, accountId) {
  if (!image || image.State !== 'available') throw new CliError('Golden AMI is not available');
  const tags = tagsFromAws(image.Tags);
  if (image.OwnerId !== accountId
    || image.Architecture !== CONFIG.architecture
    || tags.ManagedBy !== CONFIG.managedBy
    || tags.Purpose !== 'golden'
    || !image.RootDeviceName) {
    throw new CliError(`AMI ${image.ImageId} is not an ${CONFIG.architecture} Preview golden AMI owned by this account`);
  }
  return image;
}

export function buildRunInstancesArgs({ amiId, clientToken, rootDeviceName, tags, userData }) {
  const resourceTags = (name) => tagsToAws({ ...tags, Name: name });
  return [
    'ec2', 'run-instances',
    '--image-id', amiId,
    '--instance-type', CONFIG.workspaceInstanceType,
    ...buildCreditSpecificationArgs(CONFIG.workspaceInstanceType),
    '--count', '1',
    '--client-token', clientToken,
    '--network-interfaces', JSON.stringify([{
      AssociatePublicIpAddress: true,
      DeleteOnTermination: true,
      DeviceIndex: 0,
      Groups: [CONFIG.securityGroupId],
      SubnetId: CONFIG.subnetId,
    }]),
    '--iam-instance-profile', JSON.stringify({ Name: CONFIG.instanceProfile }),
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
        VolumeInitializationRate: CONFIG.volumeInitializationRate,
        VolumeSize: CONFIG.volumeSize,
        VolumeType: 'gp3',
      },
    }]),
    '--instance-initiated-shutdown-behavior', 'terminate',
    '--disable-api-stop',
    '--tag-specifications', JSON.stringify([
      { ResourceType: 'instance', Tags: resourceTags(tags.Name) },
      { ResourceType: 'volume', Tags: resourceTags(`${tags.Name}-root`) },
      { ResourceType: 'network-interface', Tags: resourceTags(`${tags.Name}-primary`) },
    ]),
    '--user-data', userData,
  ];
}

export function workspaceResourceIds(instance) {
  return [
    instance.InstanceId,
    ...(instance.BlockDeviceMappings ?? []).map(({ Ebs }) => Ebs?.VolumeId),
    ...(instance.NetworkInterfaces ?? []).map(({ NetworkInterfaceId }) => NetworkInterfaceId),
  ].filter(Boolean);
}

// The 90-minute shutdown is the only TTL fallback if bootstrap never finishes; bootstrap reschedules it.
export function buildBootstrapUserData({ hostname, expiration }) {
  return [
    '#!/bin/sh',
    'shutdown -h +90',
    `/opt/duranta-preview/app/tools/preview/bootstrap.sh --hostname ${hostname} --expires-at ${expiration} || { logger -t duranta-preview "bootstrap failed"; shutdown -h now; exit 1; }`,
    '',
  ].join('\n');
}

export function minutesUntil(expiration, now = new Date()) {
  const deadline = new Date(expiration);
  if (!Number.isFinite(deadline.getTime())) throw new CliError(`Invalid expiration: ${expiration}`);
  return Math.max(1, Math.ceil((deadline.getTime() - now.getTime()) / 60000));
}

function dnsRecordSets(hostname, publicIp) {
  return [hostname, `*.${hostname}`].map((Name) => ({
    Name,
    Type: 'A',
    TTL: 60,
    ResourceRecords: [{ Value: publicIp }],
  }));
}

export function buildDnsChanges(action, hostname, publicIp) {
  if (!['UPSERT', 'DELETE'].includes(action)) throw new CliError(`Unsupported DNS action: ${action}`);
  return dnsRecordSets(hostname, publicIp).map((ResourceRecordSet) => ({ Action: action, ResourceRecordSet }));
}

export function buildRoute53ChangeArgs(changes, comment) {
  return [
    'route53', 'change-resource-record-sets',
    '--hosted-zone-id', CONFIG.hostedZoneId,
    '--change-batch', JSON.stringify({ Comment: comment, Changes: changes }),
  ];
}

export function buildSsmProxyCommand() {
  return `aws --profile ${CONFIG.profile} --region ${CONFIG.region} ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p`;
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
    const result = runner('ssh-keygen', ['-y', '-f', identity], { encoding: 'utf8', timeout: 10000 });
    if (result.status !== 0 || !String(result.stdout).trim()) {
      throw new CliError(`Could not derive a public key from ${identity}`);
    }
    return { publicKey: result.stdout.trim(), identityFile: identity };
  }

  const result = runner('ssh-add', ['-L'], { encoding: 'utf8', timeout: 10000 });
  if (result.status !== 0) throw new CliError('No SSH key is available; run ssh-add or pass --identity <key>');
  const publicKey = String(result.stdout).split('\n').map((line) => line.trim()).find(Boolean);
  if (!publicKey) throw new CliError('ssh-agent has no public keys');
  return { publicKey, identityFile: null };
}

export function buildSendSshPublicKeyArgs(instance, publicKey) {
  return [
    'ec2-instance-connect', 'send-ssh-public-key',
    '--instance-id', instance.InstanceId,
    '--availability-zone', instance.Placement.AvailabilityZone,
    '--instance-os-user', CONFIG.sshUser,
    '--ssh-public-key', publicKey,
  ];
}

export function buildSshArgs(instance, identityFile, remoteArgs = [], { forwardAgent = false } = {}) {
  const args = [
    forwardAgent ? '-A' : '-a',
    '-o', `ProxyCommand=${buildSsmProxyCommand()}`,
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', 'ConnectTimeout=20',
  ];
  if (identityFile) args.push('-o', 'IdentitiesOnly=yes', '-i', identityFile);
  args.push(`${CONFIG.sshUser}@${instance.InstanceId}`, ...remoteArgs);
  return args;
}
