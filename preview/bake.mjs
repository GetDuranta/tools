#!/usr/bin/env node

import { randomUUID } from 'node:crypto';
import { execFile, spawn } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { promisify } from 'node:util';

import { CONFIG } from './config.mjs';
import { buildAuditTags, buildCreditSpecificationArgs, tagsToAws } from './lib.mjs';

const execFileAsync = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));

const HELP = `Build the Duranta Preview golden AMI

Usage:
  bake.mjs bake [--identity <private-key>]

Options:
  --identity <private-key>  SSH identity loaded in ssh-agent
  --help                    Show help
`;

export function parseArgs(argv) {
  let command = null;
  let commandSeen = false;
  let help = false;
  let identity = null;

  for (let index = 0; index < argv.length; index += 1) {
    const token = argv[index];
    if (token === '-h' || token === '--help') {
      help = true;
    } else if (token === '--identity' || token.startsWith('--identity=')) {
      const inline = token.startsWith('--identity=') ? token.slice('--identity='.length) : null;
      const value = inline ?? argv[++index];
      if (!value || value.startsWith('--')) throw new Error('--identity requires a value');
      identity = resolve(value);
    } else if (token.startsWith('--')) {
      throw new Error(`Unknown option: ${token}`);
    } else if (commandSeen) {
      throw new Error(`Unexpected argument: ${token}`);
    } else {
      command = token;
      commandSeen = true;
    }
  }

  if (command && command !== 'bake') throw new Error(`Unknown command: ${command}`);
  return { command, help, identity };
}

function awsArgs(args) {
  return [
    '--profile', CONFIG.profile,
    '--region', CONFIG.region,
    '--no-cli-pager',
    '--output', 'json',
    ...args,
  ];
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

async function aws(args, options) {
  const { stdout } = await run('aws', awsArgs(args), options);
  return stdout ? JSON.parse(stdout) : {};
}

export async function waitForImageAvailable(imageId, options = {}) {
  const awsClient = options.aws ?? aws;
  const sleep = options.sleep ?? ((delayMs) => new Promise((resolvePromise) => {
    setTimeout(resolvePromise, delayMs);
  }));
  const intervalMs = options.intervalMs ?? 15_000;
  const maxAttempts = options.maxAttempts ?? 240;
  let lastState = 'unknown';

  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    let response;
    try {
      response = await awsClient(['ec2', 'describe-images', '--image-ids', imageId]);
    } catch (error) {
      const message = String(error?.message ?? error);
      const retryable = /InvalidAMIID\.NotFound|RequestLimitExceeded|Throttl|ServiceUnavailable|InternalError|RequestTimeout/i.test(message);
      if (!retryable) throw error;
      lastState = 'temporarily unavailable';
      if (attempt < maxAttempts) await sleep(intervalMs);
      continue;
    }
    const image = response.Images?.find((candidate) => candidate.ImageId === imageId);
    lastState = image?.State ?? 'missing';
    if (lastState === 'available') return image;
    if (lastState !== 'pending' && lastState !== 'missing') {
      throw new Error(`AMI ${imageId} entered terminal state: ${lastState}`);
    }
    if (attempt < maxAttempts) await sleep(intervalMs);
  }

  throw new Error(`AMI ${imageId} was not available after ${maxAttempts} attempts (last state: ${lastState})`);
}

export function assertExpectedAccount(identity) {
  if (identity?.Account !== CONFIG.accountId) {
    throw new Error(`AWS profile ${CONFIG.profile} is not the Duranta Preview account`);
  }
  return identity;
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

async function getParameter(name, awsClient = aws) {
  const response = await awsClient(['ssm', 'get-parameter', '--name', name]);
  const value = response.Parameter?.Value;
  if (!value) throw new Error(`SSM parameter ${name} is empty`);
  return value;
}

async function optionalParameter(name, awsClient = aws) {
  try {
    return await getParameter(name, awsClient);
  } catch (error) {
    if (String(error.message).includes('ParameterNotFound')) return null;
    throw error;
  }
}

function tagsFromAws(tags = []) {
  return Object.fromEntries(tags.map(({ Key, Value }) => [Key, Value]));
}

async function listManagedImages(awsClient = aws) {
  const response = await awsClient([
    'ec2', 'describe-images',
    '--owners', 'self',
    '--filters',
    `Name=tag:ManagedBy,Values=${CONFIG.managedBy}`,
    'Name=tag:Purpose,Values=golden',
    `Name=architecture,Values=${CONFIG.architecture}`,
  ]);
  return managedImagesForArchitecture(response.Images ?? [])
    .sort((left, right) => String(right.CreationDate).localeCompare(String(left.CreationDate)));
}

export function managedImagesForArchitecture(images) {
  return images.filter((image) => {
    const tags = tagsFromAws(image.Tags);
    return image.Architecture === CONFIG.architecture
      && tags.ManagedBy === CONFIG.managedBy
      && tags.Purpose === 'golden';
  });
}

export function selectPruneCandidates(images, protectedImageId) {
  const ordered = [...images]
    .sort((left, right) => String(right.CreationDate).localeCompare(String(left.CreationDate)));
  return ordered.slice(CONFIG.keepAmis).filter(({ ImageId }) => ImageId !== protectedImageId);
}

async function deleteImage(image, awsClient = aws) {
  await awsClient([
    'ec2', 'deregister-image',
    '--image-id', image.ImageId,
    '--delete-associated-snapshots',
  ]);
}

async function pruneOldImages(protectedImageId) {
  const images = await listManagedImages();
  for (const image of selectPruneCandidates(images, protectedImageId)) {
    await deleteImage(image);
  }
}

export async function cleanupUnpublishedImage(imageId, options = {}) {
  const awsClient = options.aws ?? aws;
  const warn = options.warn ?? console.warn;
  let pointer;
  try {
    pointer = await optionalParameter(CONFIG.goldenAmiParameter, awsClient);
  } catch (error) {
    warn(`Could not verify the golden AMI pointer; leaving ${imageId}: ${error.message}`);
    return false;
  }
  if (pointer === imageId) return false;

  let image;
  try {
    const response = await awsClient(['ec2', 'describe-images', '--image-ids', imageId]);
    image = response.Images?.find((candidate) => candidate.ImageId === imageId);
  } catch (error) {
    warn(`Could not inspect unpublished AMI ${imageId}: ${error.message}`);
    return false;
  }
  if (!image) return true;

  try {
    await awsClient([
      'ec2', 'deregister-image',
      '--image-id', imageId,
      '--delete-associated-snapshots',
    ]);
    return true;
  } catch (error) {
    warn(`Could not deregister unpublished AMI ${imageId}: ${error.message}`);
    return false;
  }
}

export function buildAmiName(date, shortSha) {
  const stamp = date.toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 13);
  if (!/^[0-9a-f]{7,40}$/i.test(shortSha)) throw new Error('Invalid source commit');
  return `duranta-preview-${CONFIG.architecture}-main-${stamp}-${shortSha.slice(0, 8).toLowerCase()}`;
}

export function buildBuilderArgs(baseAmi, rootDeviceName, clientToken, auditTags) {
  const tags = tagsToAws({
    Name: 'duranta-preview-ami-builder',
    ManagedBy: CONFIG.managedBy,
    Purpose: 'ami-builder',
    ...auditTags,
  });
  return [
    'ec2', 'run-instances',
    '--image-id', baseAmi,
    '--instance-type', CONFIG.builderInstanceType,
    ...buildCreditSpecificationArgs(CONFIG.builderInstanceType),
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
      HttpPutResponseHopLimit: 1,
      HttpTokens: 'required',
    }),
    '--block-device-mappings', JSON.stringify([{
      DeviceName: rootDeviceName,
      Ebs: {
        DeleteOnTermination: true,
        Encrypted: true,
        VolumeSize: CONFIG.volumeSize,
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
    '--tag-specifications', JSON.stringify([
      { ResourceType: 'instance', Tags: tags },
      { ResourceType: 'volume', Tags: tags },
      { ResourceType: 'network-interface', Tags: tags },
    ]),
  ];
}

export function buildCreateImageArgs(instanceId, name, shortSha, auditTags) {
  const tags = tagsToAws({
    Name: name,
    ManagedBy: CONFIG.managedBy,
    Purpose: 'golden',
    Architecture: CONFIG.architecture,
    SourceCommit: shortSha,
    ...auditTags,
  });
  return [
    'ec2', 'create-image',
    '--instance-id', instanceId,
    '--name', name,
    '--description', `Duranta Preview golden AMI from main ${shortSha}`,
    '--tag-specifications', JSON.stringify([
      { ResourceType: 'image', Tags: tags },
      { ResourceType: 'snapshot', Tags: tags },
    ]),
  ];
}

export function validateBaseAmi(image, baseAmi) {
  if (!image?.RootDeviceName) throw new Error(`Base AMI ${baseAmi} does not have a root device`);
  if (image.Architecture !== CONFIG.architecture) {
    throw new Error(`Base AMI ${baseAmi} is ${image.Architecture ?? 'unknown'}, expected ${CONFIG.architecture}`);
  }
  return image;
}

async function resolveBaseAmi() {
  const baseAmi = await getParameter(CONFIG.baseAmiParameter);
  const response = await aws(['ec2', 'describe-images', '--image-ids', baseAmi]);
  const image = validateBaseAmi(response.Images?.[0], baseAmi);
  return { baseAmi, rootDeviceName: image.RootDeviceName };
}

async function publicKey(identity) {
  const { stdout } = await run('ssh-add', ['-L']);
  const keys = stdout.split('\n').map((line) => line.trim()).filter(Boolean);
  if (!keys.length) throw new Error('ssh-agent has no identities; run ssh-add first');
  if (!identity) return { value: keys[0], identityArgs: [] };

  const publicPath = identity.endsWith('.pub') ? identity : `${identity}.pub`;
  let value;
  try {
    value = (await readFile(publicPath, 'utf8')).trim();
  } catch {
    value = (await run('ssh-keygen', ['-y', '-f', identity])).stdout;
  }
  const body = value.split(/\s+/).slice(0, 2).join(' ');
  if (!keys.some((key) => key.startsWith(body))) {
    throw new Error(`Identity ${identity} is not loaded in ssh-agent`);
  }
  const privatePath = identity.endsWith('.pub') ? identity.slice(0, -4) : identity;
  return { value, identityArgs: ['-o', 'IdentitiesOnly=yes', '-i', privatePath] };
}

function proxyCommand() {
  return `aws --profile ${CONFIG.profile} --region ${CONFIG.region} ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p`;
}

function sshArgs(instanceId, identityArgs, knownHosts, remoteCommand) {
  const args = [
    '-A',
    '-o', `ProxyCommand=${proxyCommand()}`,
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', `UserKnownHostsFile=${knownHosts}`,
    '-o', 'ConnectTimeout=30',
    ...identityArgs,
    `${CONFIG.sshUser}@${instanceId}`,
  ];
  if (remoteCommand) args.push(remoteCommand);
  return args;
}

async function authorizeSsh(instance, key) {
  const response = await aws([
    'ec2-instance-connect', 'send-ssh-public-key',
    '--instance-id', instance.InstanceId,
    '--availability-zone', instance.Placement.AvailabilityZone,
    '--instance-os-user', CONFIG.sshUser,
    '--ssh-public-key', key,
  ]);
  if (!response.Success) throw new Error(`EC2 Instance Connect rejected the key for ${instance.InstanceId}`);
}

async function waitForSsm(instanceId) {
  const deadline = Date.now() + 20 * 60_000;
  while (Date.now() < deadline) {
    const response = await aws([
      'ssm', 'describe-instance-information',
      '--filters', `Key=InstanceIds,Values=${instanceId}`,
    ]);
    if (response.InstanceInformationList?.[0]?.PingStatus === 'Online') return;
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 5_000));
  }
  throw new Error(`Instance ${instanceId} did not register with SSM within 20 minutes`);
}

async function spawnChecked(command, args) {
  await new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { stdio: 'inherit' });
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code === 0) resolvePromise();
      else reject(new Error(`${command} exited with ${code ?? signal}`));
    });
  });
}

async function uploadRemote(instance, key, identityArgs, knownHosts) {
  await authorizeSsh(instance, key);
  const tar = spawn('tar', ['-C', join(here, 'remote'), '-cf', '-', '.'], { stdio: ['ignore', 'pipe', 'inherit'] });
  const ssh = spawn('ssh', sshArgs(
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

async function sshExec(instance, key, identityArgs, knownHosts, remoteCommand, capture = false) {
  await authorizeSsh(instance, key);
  const args = sshArgs(instance.InstanceId, identityArgs, knownHosts, remoteCommand);
  if (capture) return (await run('ssh', args, { timeout: 30 * 60_000 })).stdout;
  await spawnChecked('ssh', args);
  return '';
}

async function terminateBuilder(instanceId) {
  let instance;
  try {
    const response = await aws(['ec2', 'describe-instances', '--instance-ids', instanceId]);
    instance = response.Reservations?.[0]?.Instances?.[0];
  } catch (error) {
    if (String(error.message).includes('InvalidInstanceID.NotFound')) return;
    throw error;
  }
  const state = instance?.State?.Name;
  if (!instance || state === 'terminated') return;
  if (state !== 'shutting-down') {
    await aws(['ec2', 'modify-instance-attribute', '--instance-id', instanceId, '--no-disable-api-stop']);
    await aws(['ec2', 'terminate-instances', '--instance-ids', instanceId]);
  }
  await run('aws', awsArgs(['ec2', 'wait', 'instance-terminated', '--instance-ids', instanceId]), {
    timeout: 15 * 60_000,
  });
}

async function bake(identity) {
  await validateLocalTools();
  const caller = assertExpectedAccount(await aws(['sts', 'get-caller-identity']));
  const { baseAmi, rootDeviceName } = await resolveBaseAmi();
  const key = await publicKey(identity);
  const temporary = await mkdtemp(join(tmpdir(), 'duranta-preview-bake-'));
  const knownHosts = join(temporary, 'known_hosts');
  if (!key.identityArgs.length) {
    const selectedKey = join(temporary, 'selected-key.pub');
    await writeFile(selectedKey, `${key.value}\n`, { mode: 0o600 });
    key.identityArgs.push('-o', 'IdentitiesOnly=yes', '-i', selectedKey);
  }

  let instanceId = null;
  let imageId = null;
  let published = false;
  try {
    const launch = await aws(buildBuilderArgs(
      baseAmi,
      rootDeviceName,
      randomUUID(),
      buildAuditTags(caller),
    ), { timeout: 180_000 });
    instanceId = launch.Instances?.[0]?.InstanceId;
    if (!instanceId) throw new Error('run-instances did not return an instance ID');

    await run('aws', awsArgs(['ec2', 'wait', 'instance-running', '--instance-ids', instanceId]), {
      timeout: 15 * 60_000,
    });
    const described = await aws(['ec2', 'describe-instances', '--instance-ids', instanceId]);
    const instance = described.Reservations?.[0]?.Instances?.[0];
    if (!instance?.Placement?.AvailabilityZone) throw new Error(`Could not describe builder ${instanceId}`);

    await waitForSsm(instanceId);
    await uploadRemote(instance, key.value, key.identityArgs, knownHosts);
    await sshExec(
      instance,
      key.value,
      key.identityArgs,
      knownHosts,
      'sudo --preserve-env=SSH_AUTH_SOCK bash /tmp/duranta-preview-remote/provision.sh',
    );
    const shortSha = await sshExec(
      instance,
      key.value,
      key.identityArgs,
      knownHosts,
      'git -C /opt/duranta-preview/app rev-parse --short=8 HEAD',
      true,
    );
    const name = buildAmiName(new Date(), shortSha);
    const created = await aws(buildCreateImageArgs(
      instanceId,
      name,
      shortSha,
      buildAuditTags(caller),
    ));
    imageId = created.ImageId;
    if (!imageId) throw new Error('create-image did not return an AMI ID');

    await waitForImageAvailable(imageId);
    await aws([
      'ssm', 'put-parameter',
      '--name', CONFIG.goldenAmiParameter,
      '--type', 'String',
      '--value', imageId,
      '--overwrite',
    ]);
    published = true;
    console.log(`Published ${name} (${imageId})`);
  } finally {
    if (instanceId) {
      try {
        await terminateBuilder(instanceId);
      } catch (error) {
        console.error(`Failed to terminate builder ${instanceId}: ${error.message}`);
        process.exitCode = 1;
      }
    }
    if (imageId && !published) await cleanupUnpublishedImage(imageId);
    await rm(temporary, { recursive: true, force: true });
  }

  await pruneOldImages(imageId);
}

export async function main(argv = process.argv.slice(2)) {
  const options = parseArgs(argv);
  if (options.help || !options.command) {
    console.log(HELP);
    return;
  }
  await bake(options.identity);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
