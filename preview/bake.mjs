#!/usr/bin/env node

import { execFile, spawn } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { promisify } from 'node:util';

import { validateCpuInstanceType } from './lib.mjs';

export { validateCpuInstanceType };

const execFileAsync = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));

const DEFAULTS = Object.freeze({
  profile: 'preview',
  region: 'us-west-2',
  instanceType: 'm7i.4xlarge',
  volumeSize: 200,
  sshUser: 'ec2-user',
  keep: 2,
  baseAmiParameter: '/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64',
  goldenAmiParameter: '/duranta-preview/golden-ami-id',
  subnetParameter: '/duranta-preview/subnet-id',
  securityGroupParameter: '/duranta-preview/security-group-id',
  instanceProfileParameter: '/duranta-preview/instance-profile',
});

const HELP = `Duranta preview golden AMI lifecycle

Usage:
  bake.mjs bake [options] [--apply]
  bake.mjs list [options] [--json]
  bake.mjs prune [options] [--keep <count>] [--apply]

Without --apply, bake and prune perform read-only validation and print a plan.

Options:
  --profile <name>                 AWS profile (default: preview)
  --region <region>                AWS region (default: us-west-2)
  --instance-type <type>           Builder type (default: m7i.4xlarge)
  --volume-size <GiB>              Builder root volume (default: 200)
  --base-ami <ami-id>              Override the public Amazon Linux 2023 parameter
  --subnet-id <subnet-id>          Override /duranta-preview/subnet-id
  --security-group-id <sg-id>      Override /duranta-preview/security-group-id
  --instance-profile <name>        Override /duranta-preview/instance-profile
  --identity <private-key>         SSH identity loaded in ssh-agent
  --golden-parameter <name>        Golden AMI SSM pointer
  --keep <count>                   AMIs retained by prune (default: 2)
  --json                           JSON output for list
  --apply                          Perform writes
`;

const BOOLEAN_OPTIONS = new Set(['apply', 'help', 'json']);
const VALUE_OPTIONS = new Set([
  'profile', 'region', 'instance-type', 'volume-size', 'base-ami',
  'base-ami-parameter', 'subnet-id', 'security-group-id', 'instance-profile',
  'identity', 'golden-parameter', 'keep', 'ssh-user',
]);

function camel(value) {
  return value.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
}

export function parseArgs(argv, env = process.env) {
  const options = {};
  const positionals = [];
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
    const [name, inline] = token.slice(2).split(/=(.*)/s, 2);
    if (BOOLEAN_OPTIONS.has(name)) {
      if (inline !== undefined) throw new Error(`--${name} does not accept a value`);
      options[camel(name)] = true;
      continue;
    }
    if (!VALUE_OPTIONS.has(name)) throw new Error(`Unknown option: --${name}`);
    const value = inline ?? argv[++index];
    if (!value || value.startsWith('--')) throw new Error(`--${name} requires a value`);
    options[camel(name)] = value;
  }

  const command = positionals.shift() ?? 'bake';
  if (positionals.length) throw new Error(`Unexpected argument: ${positionals[0]}`);
  if (!['bake', 'list', 'prune'].includes(command)) throw new Error(`Unknown command: ${command}`);

  const positiveInteger = (value, name) => {
    const parsed = Number(value);
    if (!Number.isInteger(parsed) || parsed <= 0) throw new Error(`${name} must be a positive integer`);
    return parsed;
  };
  const nonnegativeInteger = (value, name) => {
    const parsed = Number(value);
    if (!Number.isInteger(parsed) || parsed < 0) throw new Error(`${name} must be a non-negative integer`);
    return parsed;
  };

  return {
    command,
    apply: options.apply ?? false,
    json: options.json ?? false,
    help: options.help ?? false,
    profile: options.profile ?? env.DURANTA_PREVIEW_PROFILE ?? DEFAULTS.profile,
    region: options.region ?? env.DURANTA_PREVIEW_REGION ?? DEFAULTS.region,
    instanceType: options.instanceType ?? env.DURANTA_PREVIEW_INSTANCE_TYPE ?? DEFAULTS.instanceType,
    volumeSize: positiveInteger(options.volumeSize ?? env.DURANTA_PREVIEW_VOLUME_SIZE ?? DEFAULTS.volumeSize, 'volume size'),
    baseAmi: options.baseAmi ?? env.DURANTA_PREVIEW_BASE_AMI ?? null,
    baseAmiParameter: options.baseAmiParameter ?? env.DURANTA_PREVIEW_BASE_AMI_PARAMETER ?? DEFAULTS.baseAmiParameter,
    subnetId: options.subnetId ?? env.DURANTA_PREVIEW_SUBNET_ID ?? null,
    securityGroupId: options.securityGroupId ?? env.DURANTA_PREVIEW_SECURITY_GROUP_ID ?? null,
    instanceProfile: options.instanceProfile ?? env.DURANTA_PREVIEW_INSTANCE_PROFILE ?? null,
    identity: options.identity ? resolve(options.identity) : null,
    goldenAmiParameter: options.goldenParameter ?? env.DURANTA_PREVIEW_GOLDEN_AMI_PARAMETER ?? DEFAULTS.goldenAmiParameter,
    keep: nonnegativeInteger(options.keep ?? DEFAULTS.keep, 'keep'),
    sshUser: options.sshUser ?? env.DURANTA_PREVIEW_SSH_USER ?? DEFAULTS.sshUser,
  };
}

function awsArgs(config, args) {
  return ['--profile', config.profile, '--region', config.region, '--no-cli-pager', '--output', 'json', ...args];
}

async function run(command, args, options = {}) {
  try {
    const result = await execFileAsync(command, args, {
      encoding: 'utf8',
      maxBuffer: 32 * 1024 * 1024,
      timeout: options.timeout ?? 120_000,
      ...options,
    });
    return { stdout: result.stdout.trim(), stderr: result.stderr.trim() };
  } catch (error) {
    const detail = String(error.stderr || error.stdout || error.message).trim();
    throw new Error(`${command} failed${detail ? `: ${detail}` : ''}`);
  }
}

async function aws(config, args, options) {
  const { stdout } = await run('aws', awsArgs(config, args), options);
  return stdout ? JSON.parse(stdout) : {};
}

async function validateLocalTools() {
  const installed = async (command, args) => {
    try {
      await execFileAsync(command, args, { encoding: 'utf8', timeout: 10_000 });
    } catch (error) {
      if (error.code === 'ENOENT') throw new Error(`${command} is not installed`);
    }
  };
  await Promise.all([
    installed('aws', ['--version']),
    installed('ssh', ['-V']),
    installed('ssh-add', ['-L']),
    installed('session-manager-plugin', []),
    installed('tar', ['--version']),
  ]);
}

async function getParameter(config, name) {
  const response = await aws(config, ['ssm', 'get-parameter', '--name', name]);
  const value = response.Parameter?.Value;
  if (!value) throw new Error(`SSM parameter ${name} is empty`);
  return value;
}

async function resolveBakeConfig(config) {
  const [baseAmi, subnetId, securityGroupId, instanceProfile] = await Promise.all([
    config.baseAmi ?? getParameter(config, config.baseAmiParameter),
    config.subnetId ?? getParameter(config, DEFAULTS.subnetParameter),
    config.securityGroupId ?? getParameter(config, DEFAULTS.securityGroupParameter),
    config.instanceProfile ?? getParameter(config, DEFAULTS.instanceProfileParameter),
  ]);
  const images = await aws(config, ['ec2', 'describe-images', '--image-ids', baseAmi]);
  const image = images.Images?.[0];
  if (!image?.RootDeviceName) throw new Error(`Base AMI ${baseAmi} does not have a root device`);
  return { ...config, baseAmi, subnetId, securityGroupId, instanceProfile, rootDeviceName: image.RootDeviceName };
}

async function validateStandardQuota(config) {
  const [types, quota] = await Promise.all([
    aws(config, ['ec2', 'describe-instance-types', '--instance-types', config.instanceType]),
    aws(config, [
      'service-quotas', 'get-service-quota', '--service-code', 'ec2',
      '--quota-code', 'L-1216C47A',
    ]),
  ]);
  const required = validateCpuInstanceType(config.instanceType, types.InstanceTypes?.[0]);
  const available = quota.Quota?.Value;
  if (!Number.isFinite(available)) {
    throw new Error(`Could not resolve Standard On-Demand quota for ${config.instanceType}`);
  }
  if (available < required) {
    throw new Error(`Standard On-Demand quota is ${available} vCPUs; ${config.instanceType} requires ${required} (quota L-1216C47A)`);
  }
  return { available, required };
}

function tagsFromAws(tags = []) {
  return Object.fromEntries(tags.map(({ Key, Value }) => [Key, Value]));
}

function imageSummary(image) {
  const tags = tagsFromAws(image.Tags);
  return {
    imageId: image.ImageId,
    name: image.Name,
    creationDate: image.CreationDate,
    sourceCommit: tags.SourceCommit ?? '',
    state: image.State,
    snapshots: (image.BlockDeviceMappings ?? []).flatMap(({ Ebs }) => Ebs?.SnapshotId ? [Ebs.SnapshotId] : []),
  };
}

async function listManagedImages(config) {
  const response = await aws(config, [
    'ec2', 'describe-images', '--owners', 'self', '--filters',
    'Name=tag:ManagedBy,Values=duranta-preview',
    'Name=tag:Purpose,Values=golden',
  ]);
  return (response.Images ?? [])
    .filter((image) => {
      const tags = tagsFromAws(image.Tags);
      return tags.ManagedBy === 'duranta-preview' && tags.Purpose === 'golden';
    })
    .sort((left, right) => right.CreationDate.localeCompare(left.CreationDate));
}

export function selectPruneCandidates(images, keep, protectedImageId = null) {
  const ordered = [...images].sort((left, right) => right.CreationDate.localeCompare(left.CreationDate));
  return ordered.slice(keep).filter(({ ImageId }) => ImageId !== protectedImageId);
}

export function buildAmiName(date, shortSha) {
  const stamp = date.toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 13);
  if (!/^[0-9a-f]{7,40}$/i.test(shortSha)) throw new Error('Invalid source commit');
  return `duranta-preview-main-${stamp}-${shortSha.slice(0, 8).toLowerCase()}`;
}

function printImages(images) {
  const rows = images.map(imageSummary);
  if (!rows.length) {
    console.log('No managed golden AMIs found.');
    return;
  }
  for (const row of rows) {
    console.log(`${row.imageId}  ${row.creationDate}  ${row.name}  ${row.sourceCommit}`);
  }
}

async function listCommand(config) {
  const images = await listManagedImages(config);
  if (config.json) console.log(JSON.stringify(images.map(imageSummary), null, 2));
  else printImages(images);
}

async function currentGoldenAmi(config) {
  try {
    return await getParameter(config, config.goldenAmiParameter);
  } catch (error) {
    if (String(error.message).includes('ParameterNotFound')) return null;
    throw error;
  }
}

async function safeSnapshotIds(config, image, ownerId) {
  const ids = imageSummary(image).snapshots;
  if (!ids.length) return [];
  const response = await aws(config, ['ec2', 'describe-snapshots', '--snapshot-ids', ...ids]);
  return (response.Snapshots ?? []).filter((snapshot) => {
    const tags = tagsFromAws(snapshot.Tags);
    return snapshot.OwnerId === ownerId
      && tags.ManagedBy === 'duranta-preview'
      && tags.Purpose === 'golden';
  }).map(({ SnapshotId }) => SnapshotId);
}

async function snapshotIsUnreferenced(config, snapshotId) {
  const response = await aws(config, [
    'ec2', 'describe-images', '--owners', 'self', '--filters',
    `Name=block-device-mapping.snapshot-id,Values=${snapshotId}`,
  ]);
  return (response.Images ?? []).length === 0;
}

async function pruneCommand(config) {
  const ownerId = (await aws(config, ['sts', 'get-caller-identity'])).Account;
  const images = await listManagedImages(config);
  const protectedImageId = await currentGoldenAmi(config);
  const candidates = selectPruneCandidates(images, config.keep, protectedImageId);
  console.log(`Managed AMIs: ${images.length}; keep newest: ${config.keep}; protected pointer: ${protectedImageId ?? 'none'}`);
  printImages(candidates);
  if (!config.apply) {
    console.log('Dry run. Re-run with --apply to deregister these AMIs and delete their unreferenced tagged snapshots.');
    return;
  }
  for (const image of candidates) {
    const snapshots = await safeSnapshotIds(config, image, ownerId);
    await aws(config, ['ec2', 'deregister-image', '--image-id', image.ImageId]);
    for (const snapshotId of snapshots) {
      if (await snapshotIsUnreferenced(config, snapshotId)) {
        await aws(config, ['ec2', 'delete-snapshot', '--snapshot-id', snapshotId]);
      } else {
        console.warn(`Kept referenced snapshot ${snapshotId}`);
      }
    }
  }
}

async function publicKey(config) {
  const { stdout } = await run('ssh-add', ['-L']);
  const keys = stdout.split('\n').map((line) => line.trim()).filter(Boolean);
  if (!keys.length) throw new Error('ssh-agent has no identities; run ssh-add first');
  if (!config.identity) return { value: keys[0], identityArgs: [] };
  const publicPath = config.identity.endsWith('.pub') ? config.identity : `${config.identity}.pub`;
  let value;
  try {
    value = (await readFile(publicPath, 'utf8')).trim();
  } catch {
    value = (await run('ssh-keygen', ['-y', '-f', config.identity])).stdout;
  }
  const body = value.split(/\s+/).slice(0, 2).join(' ');
  if (!keys.some((key) => key.startsWith(body))) {
    throw new Error(`Identity ${config.identity} is not loaded in ssh-agent; run ssh-add ${config.identity}`);
  }
  const privatePath = config.identity.endsWith('.pub') ? config.identity.slice(0, -4) : config.identity;
  return { value, identityArgs: ['-o', 'IdentitiesOnly=yes', '-i', privatePath] };
}

function proxyCommand(config) {
  const safe = /^[a-zA-Z0-9_+=,.@-]+$/;
  if (!safe.test(config.profile) || !safe.test(config.region)) throw new Error('Unsafe profile or region');
  return `aws --profile ${config.profile} --region ${config.region} ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p`;
}

function sshArgs(config, instanceId, identityArgs, knownHosts, remoteCommand) {
  const args = [
    '-A', '-o', `ProxyCommand=${proxyCommand(config)}`,
    '-o', 'StrictHostKeyChecking=accept-new', '-o', `UserKnownHostsFile=${knownHosts}`,
    '-o', 'ConnectTimeout=30', ...identityArgs,
    `${config.sshUser}@${instanceId}`,
  ];
  if (remoteCommand) args.push(remoteCommand);
  return args;
}

async function authorizeSsh(config, instance, key) {
  await aws(config, [
    'ec2-instance-connect', 'send-ssh-public-key',
    '--instance-id', instance.InstanceId,
    '--availability-zone', instance.Placement.AvailabilityZone,
    '--instance-os-user', config.sshUser,
    '--ssh-public-key', key,
  ]);
}

async function waitForSsm(config, instanceId) {
  const deadline = Date.now() + 20 * 60_000;
  while (Date.now() < deadline) {
    const response = await aws(config, [
      'ssm', 'describe-instance-information', '--filters', `Key=InstanceIds,Values=${instanceId}`,
    ]);
    if (response.InstanceInformationList?.[0]?.PingStatus === 'Online') return;
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 5_000));
  }
  throw new Error(`Instance ${instanceId} did not register with SSM within 20 minutes`);
}

async function spawnChecked(command, args, options = {}) {
  await new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { stdio: 'inherit', ...options });
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code === 0) resolvePromise();
      else reject(new Error(`${command} exited with ${code ?? signal}`));
    });
  });
}

async function uploadRemote(config, instance, key, identityArgs, knownHosts) {
  await authorizeSsh(config, instance, key);
  const tar = spawn('tar', ['-C', join(here, 'remote'), '-cf', '-', '.'], { stdio: ['ignore', 'pipe', 'inherit'] });
  const ssh = spawn('ssh', sshArgs(
    config,
    instance.InstanceId,
    identityArgs,
    knownHosts,
    'sudo install -d -m 0755 /tmp/duranta-preview-remote && sudo tar -C /tmp/duranta-preview-remote -xf -',
  ), { stdio: ['pipe', 'inherit', 'inherit'] });
  tar.stdout.pipe(ssh.stdin);
  const wait = (child, name) => new Promise((resolvePromise, reject) => {
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code === 0) resolvePromise();
      else reject(new Error(`${name} exited with ${code ?? signal}`));
    });
  });
  await Promise.all([wait(tar, 'tar'), wait(ssh, 'ssh upload')]);
}

async function sshExec(config, instance, key, identityArgs, knownHosts, remoteCommand, capture = false) {
  await authorizeSsh(config, instance, key);
  if (capture) {
    return (await run('ssh', sshArgs(config, instance.InstanceId, identityArgs, knownHosts, remoteCommand), { timeout: 30 * 60_000 })).stdout;
  }
  await spawnChecked('ssh', sshArgs(config, instance.InstanceId, identityArgs, knownHosts, remoteCommand));
  return '';
}

async function terminateBuilder(config, instanceId) {
  let instance;
  try {
    const response = await aws(config, ['ec2', 'describe-instances', '--instance-ids', instanceId]);
    instance = response.Reservations?.[0]?.Instances?.[0];
  } catch (error) {
    if (String(error.message).includes('InvalidInstanceID.NotFound')) return;
    throw error;
  }
  const state = instance?.State?.Name;
  if (!instance || state === 'terminated') return;
  if (state !== 'shutting-down') {
    await aws(config, ['ec2', 'modify-instance-attribute', '--instance-id', instanceId, '--no-disable-api-stop']);
    await aws(config, ['ec2', 'terminate-instances', '--instance-ids', instanceId]);
  }
  await run('aws', awsArgs(config, ['ec2', 'wait', 'instance-terminated', '--instance-ids', instanceId]), { timeout: 15 * 60_000 });
}

async function bakeCommand(config) {
  await validateLocalTools();
  await aws(config, ['sts', 'get-caller-identity']);
  const resolved = await resolveBakeConfig(config);
  const quota = await validateStandardQuota(resolved);
  console.log(JSON.stringify({
    action: config.apply ? 'build-and-publish' : 'plan',
    profile: resolved.profile,
    region: resolved.region,
    baseAmi: resolved.baseAmi,
    builder: resolved.instanceType,
    volumeGiB: resolved.volumeSize,
    subnetId: resolved.subnetId,
    securityGroupId: resolved.securityGroupId,
    instanceProfile: resolved.instanceProfile,
    goldenAmiParameter: resolved.goldenAmiParameter,
    standardQuotaVcpus: quota.available,
    requiredVcpus: quota.required,
  }, null, 2));
  if (!config.apply) {
    console.log('Dry run. Re-run with --apply to launch the temporary builder and publish an AMI.');
    return;
  }

  const key = await publicKey(resolved);
  const temp = await mkdtemp(join(tmpdir(), 'duranta-preview-bake-'));
  const knownHosts = join(temp, 'known_hosts');
  if (!key.identityArgs.length) {
    const identity = join(temp, 'selected-key.pub');
    await writeFile(identity, `${key.value}\n`, { mode: 0o600 });
    key.identityArgs.push('-o', 'IdentitiesOnly=yes', '-i', identity);
  }
  let instanceId = null;
  try {
    const tags = [
      { Key: 'Name', Value: 'duranta-preview-ami-builder' },
      { Key: 'ManagedBy', Value: 'duranta-preview' },
      { Key: 'Purpose', Value: 'ami-builder' },
    ];
    const response = await aws(resolved, [
      'ec2', 'run-instances',
      '--image-id', resolved.baseAmi,
      '--instance-type', resolved.instanceType,
      '--min-count', '1', '--max-count', '1',
      '--network-interfaces', JSON.stringify([{
        AssociatePublicIpAddress: true,
        DeleteOnTermination: true,
        DeviceIndex: 0,
        Groups: [resolved.securityGroupId],
        SubnetId: resolved.subnetId,
      }]),
      '--iam-instance-profile', JSON.stringify({ Name: resolved.instanceProfile }),
      '--metadata-options', JSON.stringify({
        HttpEndpoint: 'enabled',
        HttpPutResponseHopLimit: 1,
        HttpTokens: 'required',
      }),
      '--block-device-mappings', JSON.stringify([{
        DeviceName: resolved.rootDeviceName,
        Ebs: {
          DeleteOnTermination: true,
          Encrypted: true,
          VolumeSize: resolved.volumeSize,
          VolumeType: 'gp3',
        },
      }]),
      '--user-data', [
        '#!/bin/bash',
        'systemd-run --unit=duranta-preview-builder-deadman --on-active=6h /usr/sbin/shutdown -h now',
        '',
      ].join('\n'),
      '--instance-initiated-shutdown-behavior', 'terminate',
      '--disable-api-stop',
      '--tag-specifications',
      JSON.stringify([
        { ResourceType: 'instance', Tags: tags },
        { ResourceType: 'volume', Tags: tags },
      ]),
    ]);
    instanceId = response.Instances?.[0]?.InstanceId;
    if (!instanceId) throw new Error('run-instances did not return an instance ID');
    await run('aws', awsArgs(resolved, ['ec2', 'wait', 'instance-running', '--instance-ids', instanceId]), { timeout: 15 * 60_000 });
    const described = await aws(resolved, ['ec2', 'describe-instances', '--instance-ids', instanceId]);
    const instance = described.Reservations?.[0]?.Instances?.[0];
    if (!instance?.Placement?.AvailabilityZone) throw new Error(`Could not describe builder ${instanceId}`);
    await waitForSsm(resolved, instanceId);
    await uploadRemote(resolved, instance, key.value, key.identityArgs, knownHosts);
    await sshExec(
      resolved,
      instance,
      key.value,
      key.identityArgs,
      knownHosts,
      'sudo --preserve-env=SSH_AUTH_SOCK bash /tmp/duranta-preview-remote/provision.sh',
    );
    const shortSha = await sshExec(
      resolved,
      instance,
      key.value,
      key.identityArgs,
      knownHosts,
      'git -C /opt/duranta-preview/app rev-parse --short=8 HEAD',
      true,
    );
    const name = buildAmiName(new Date(), shortSha);
    const imageTags = [
      { Key: 'Name', Value: name },
      { Key: 'ManagedBy', Value: 'duranta-preview' },
      { Key: 'Purpose', Value: 'golden' },
      { Key: 'SourceCommit', Value: shortSha },
    ];
    const created = await aws(resolved, [
      'ec2', 'create-image', '--instance-id', instanceId, '--name', name,
      '--description', `Duranta Preview golden AMI from main ${shortSha}`,
      '--tag-specifications',
      JSON.stringify([
        { ResourceType: 'image', Tags: imageTags },
        { ResourceType: 'snapshot', Tags: imageTags },
      ]),
    ]);
    const imageId = created.ImageId;
    if (!imageId) throw new Error('create-image did not return an AMI ID');
    await run('aws', awsArgs(resolved, ['ec2', 'wait', 'image-available', '--image-ids', imageId]), { timeout: 60 * 60_000 });
    await aws(resolved, [
      'ssm', 'put-parameter', '--name', resolved.goldenAmiParameter,
      '--type', 'String', '--value', imageId, '--overwrite',
    ]);
    console.log(`Published ${name} (${imageId}) to ${resolved.goldenAmiParameter}`);
    await pruneCommand({ ...resolved, apply: true, keep: resolved.keep, json: false });
  } finally {
    if (instanceId) {
      try {
        await terminateBuilder(resolved, instanceId);
      } catch (error) {
        console.error(`Failed to terminate builder ${instanceId}: ${error.message}`);
        process.exitCode = 1;
      }
    }
    await rm(temp, { recursive: true, force: true });
  }
}

export async function main(argv = process.argv.slice(2)) {
  const config = parseArgs(argv);
  if (config.help) {
    console.log(HELP);
    return;
  }
  if (config.command === 'list') await listCommand(config);
  else if (config.command === 'prune') await pruneCommand(config);
  else await bakeCommand(config);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
