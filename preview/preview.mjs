#!/usr/bin/env node

import { randomUUID } from 'node:crypto';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { CONFIG } from './config.mjs';
import {
  AwsCli,
  CliError,
  assertManagedResource,
  assertPreviewAccount,
  assertVolumeInitializationRateSupport,
  buildAuditTags,
  buildBootstrapUserData,
  buildDnsChanges,
  buildHostname,
  buildRoute53ChangeArgs,
  buildRunInstancesArgs,
  buildSendSshPublicKeyArgs,
  buildSshArgs,
  buildTags,
  countManagedInstancesForCreator,
  creatorIdFromIdentity,
  expiresAt,
  extendExpiration,
  isManagedResource,
  minutesUntil,
  normalizeDnsLabel,
  normalizeOwner,
  parseDuration,
  publicKeyFromIdentity,
  tagsFromAws,
  validateGoldenAmi,
  workspaceResourceIds,
} from './lib.mjs';

const HELP = `Duranta Preview EC2 environments

Usage:
  preview.mjs create <issue> [--owner <name>] [--ttl <duration>]
  preview.mjs list
  preview.mjs connect <name> [--identity <key>] [--forward-agent]
  preview.mjs extend <name> <duration> [--identity <key>]
  preview.mjs terminate <name> [--yes]

Fixed environment: AWS profile ${CONFIG.profile}, account ${CONFIG.accountId},
${CONFIG.region}, ${CONFIG.domain}. New machines live for ${CONFIG.ttl} by default.
`;

const BOOLEAN_OPTIONS = new Set(['forward-agent', 'help', 'yes']);
const VALUE_OPTIONS = new Set(['identity', 'owner', 'ttl']);

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
    const [name, inline] = token.slice(2).split(/=(.*)/s, 2);
    if (BOOLEAN_OPTIONS.has(name)) {
      if (inline !== undefined) throw new CliError(`--${name} does not accept a value`);
      options[optionKey(name)] = true;
      continue;
    }
    if (!VALUE_OPTIONS.has(name)) throw new CliError(`Unknown option: --${name}`);
    const value = inline ?? argv[++index];
    if (!value || value.startsWith('--')) throw new CliError(`--${name} requires a value`);
    options[optionKey(name)] = value;
  }
  return { positionals, options };
}

function allowOptions(options, names) {
  const allowed = new Set([...names, 'help']);
  const unexpected = Object.keys(options).find((name) => !allowed.has(name));
  if (unexpected) throw new CliError(`--${unexpected.replace(/[A-Z]/g, (letter) => `-${letter.toLowerCase()}`)} is not valid for this command`);
}

function flattenInstances(response) {
  return (response.Reservations ?? []).flatMap((reservation) => reservation.Instances ?? []);
}

function instanceState(instance) {
  return instance.State?.Name ?? 'unknown';
}

function getManagedInstances(aws) {
  const response = aws.run([
    'ec2', 'describe-instances',
    '--filters', JSON.stringify([
      { Name: 'tag:ManagedBy', Values: [CONFIG.managedBy] },
      { Name: 'instance-state-name', Values: ['pending', 'running', 'stopping', 'stopped'] },
    ]),
  ]);
  return flattenInstances(response).filter((instance) => {
    const tags = tagsFromAws(instance.Tags);
    return isManagedResource(instance.Tags) && tags.Hostname && tags.CreatorId;
  });
}

function findTag(instance, key) {
  return tagsFromAws(instance.Tags)[key];
}

function resolveManagedInstance(aws, name) {
  const input = String(name).replace(/\.$/, '').toLowerCase();
  let issue;
  try {
    issue = normalizeDnsLabel(name);
  } catch {}
  const matches = getManagedInstances(aws).filter((instance) => {
    const tags = tagsFromAws(instance.Tags);
    return instance.InstanceId === name
      || String(tags.Hostname ?? '').toLowerCase() === input
      || tags.Issue === issue;
  });
  if (!matches.length) throw new CliError(`Managed preview not found: ${name}`);
  if (matches.length > 1) throw new CliError(`More than one preview matches ${name}; use its hostname or instance ID`);
  return assertManagedResource(matches[0], `instance ${matches[0].InstanceId}`);
}

function callerIdentity(aws) {
  return assertPreviewAccount(aws.run(['sts', 'get-caller-identity']));
}

function inferOwner(identity, explicit) {
  if (explicit) return normalizeOwner(explicit);
  return normalizeOwner(buildAuditTags(identity).CreatedBy);
}

function resolveGoldenAmi(aws, accountId) {
  const parameter = aws.run(['ssm', 'get-parameter', '--name', CONFIG.goldenAmiParameter]);
  const imageId = parameter.Parameter?.Value;
  if (!imageId) throw new CliError(`SSM parameter ${CONFIG.goldenAmiParameter} is empty`);
  const response = aws.run(['ec2', 'describe-images', '--image-ids', imageId]);
  return validateGoldenAmi(response.Images?.[0], accountId);
}

function describeInstance(aws, instanceId) {
  const response = aws.run(['ec2', 'describe-instances', '--instance-ids', instanceId]);
  const instance = flattenInstances(response)[0];
  if (!instance) throw new CliError(`Instance not found: ${instanceId}`);
  return instance;
}

function terminateInstance(aws, instance, hostname) {
  try {
    aws.run(['ec2', 'modify-instance-attribute', '--instance-id', instance.InstanceId, '--no-disable-api-stop']);
  } catch (error) {
    console.warn(`Could not remove stop protection: ${error.message}`);
  }
  aws.run(['ec2', 'terminate-instances', '--instance-ids', instance.InstanceId]);
  if (hostname && instance.PublicIpAddress) {
    try {
      aws.run(buildRoute53ChangeArgs(
        buildDnsChanges('DELETE', hostname, instance.PublicIpAddress),
        `Delete Preview ${hostname}`,
      ));
    } catch (error) {
      console.warn(`DNS cleanup failed: ${error.message}`);
    }
  }
  aws.run(['ec2', 'wait', 'instance-terminated', '--instance-ids', instance.InstanceId], { timeout: 900000 });
}

export async function waitForPreview(hostname, timeoutMs = 45 * 60 * 1000, dependencies = {}) {
  const fetchImpl = dependencies.fetchImpl ?? fetch;
  const sleep = dependencies.sleep ?? ((delayMs) => new Promise((resolvePromise) => setTimeout(resolvePromise, delayMs)));
  const now = dependencies.now ?? Date.now;
  const deadline = now() + timeoutMs;
  let lastError = 'not attempted';
  while (now() < deadline) {
    try {
      const app = await fetchImpl(`https://${hostname}/a/`, {
        redirect: 'manual',
        signal: AbortSignal.timeout(10000),
      });
      if (app.status === 200) return;
      lastError = `public app returned HTTP ${app.status}`;
    } catch (error) {
      lastError = error.message;
    }
    await sleep(10000);
  }
  throw new CliError(`Preview was not fully ready after ${Math.ceil(timeoutMs / 60000)} minutes (${lastError})`);
}

async function createPreview(aws, issue, options) {
  const identity = callerIdentity(aws);
  assertVolumeInitializationRateSupport(aws.run([
    'ec2', 'run-instances', '--generate-cli-skeleton', 'input',
  ]));
  const creatorId = creatorIdFromIdentity(identity);
  const owner = inferOwner(identity, options.owner);
  const hostname = buildHostname(issue, owner);
  const ttl = options.ttl ?? CONFIG.ttl;
  parseDuration(ttl);

  const instances = getManagedInstances(aws);
  if (instances.some((instance) => findTag(instance, 'Hostname') === hostname)) {
    throw new CliError(`Preview already exists: ${hostname}`);
  }
  const count = countManagedInstancesForCreator(instances, creatorId);
  if (count >= CONFIG.maxInstancesPerCreator) {
    throw new CliError(`You already have ${count} active previews; the limit is ${CONFIG.maxInstancesPerCreator}`);
  }

  const image = resolveGoldenAmi(aws, identity.Account);
  const createdAt = new Date();
  const expiration = expiresAt(ttl, createdAt);
  const auditTags = buildAuditTags(identity, createdAt);
  const tags = buildTags({
    auditTags,
    issue,
    owner,
    hostname,
    expiration,
  });
  const userData = buildBootstrapUserData({ hostname, expiration });
  const launch = aws.run(buildRunInstancesArgs({
    amiId: image.ImageId,
    clientToken: randomUUID(),
    rootDeviceName: image.RootDeviceName,
    tags,
    userData,
  }), { timeout: 180000 });
  const instanceId = launch.Instances?.[0]?.InstanceId;
  if (!instanceId) throw new CliError('EC2 did not return an instance ID');

  let instance = { InstanceId: instanceId };
  console.log(`Launched ${instanceId}; waiting for the stack...`);
  try {
    aws.run(['ec2', 'wait', 'instance-running', '--instance-ids', instanceId], { timeout: 900000 });
    instance = describeInstance(aws, instanceId);
    if (!instance.PublicIpAddress) throw new CliError(`Instance ${instanceId} has no public IPv4 address`);
    const dns = aws.run(buildRoute53ChangeArgs(
      buildDnsChanges('UPSERT', hostname, instance.PublicIpAddress),
      `Create Preview ${hostname}`,
    ));
    if (dns.ChangeInfo?.Id) {
      aws.run(['route53', 'wait', 'resource-record-sets-changed', '--id', dns.ChangeInfo.Id], { timeout: 300000 });
    }
    aws.run(['ec2', 'wait', 'instance-status-ok', '--instance-ids', instanceId], { timeout: 900000 });
    await waitForPreview(hostname);
  } catch (error) {
    let cleanup = `Terminated failed Preview ${instanceId}.`;
    try {
      terminateInstance(aws, instance, hostname);
    } catch (cleanupError) {
      cleanup = `Automatic cleanup failed: ${cleanupError.message}`;
    }
    throw new CliError(`${error.message}\n${cleanup}`);
  }

  console.log(`URL: https://${hostname}/a/`);
  console.log(`SSH: preview.mjs connect ${hostname}`);
  console.log(`Expires: ${expiration}`);
}

function listPreviews(aws) {
  const rows = getManagedInstances(aws).map((instance) => {
    const tags = tagsFromAws(instance.Tags);
    return {
      name: tags.Hostname ?? '',
      state: instanceState(instance),
      instanceId: instance.InstanceId,
      publicIp: instance.PublicIpAddress ?? '',
      expiresAt: tags.ExpiresAt ?? '',
    };
  }).sort((left, right) => left.name.localeCompare(right.name));
  if (!rows.length) {
    console.log('No Preview instances found.');
    return;
  }
  const columns = ['name', 'state', 'instanceId', 'publicIp', 'expiresAt'];
  const widths = Object.fromEntries(columns.map((column) => [
    column,
    Math.max(column.length, ...rows.map((row) => String(row[column]).length)),
  ]));
  console.log(columns.map((column) => column.padEnd(widths[column])).join('  '));
  for (const row of rows) {
    console.log(columns.map((column) => String(row[column]).padEnd(widths[column])).join('  '));
  }
}

function injectSshKey(aws, instance, options) {
  if (instanceState(instance) !== 'running') {
    throw new CliError(`Instance ${instance.InstanceId} is ${instanceState(instance)}, not running`);
  }
  const key = publicKeyFromIdentity(options.identity);
  const response = aws.run(buildSendSshPublicKeyArgs(instance, key.publicKey));
  if (!response.Success) throw new CliError(`EC2 Instance Connect rejected the SSH key for ${instance.InstanceId}`);
  return key;
}

function runSsh(aws, instance, options, remoteArgs = [], { capture = false, forwardAgent = false } = {}) {
  const key = injectSshKey(aws, instance, options);
  let temporaryDirectory;
  let identityFile = key.identityFile;
  if (!identityFile) {
    temporaryDirectory = mkdtempSync(join(tmpdir(), 'duranta-preview-ssh-'));
    identityFile = join(temporaryDirectory, 'selected-key.pub');
    writeFileSync(identityFile, `${key.publicKey}\n`, { mode: 0o600 });
  }
  try {
    const result = spawnSync('ssh', buildSshArgs(instance, identityFile, remoteArgs, { forwardAgent }), capture
      ? { encoding: 'utf8', timeout: 120000 }
      : { stdio: 'inherit' });
    if (result.error) throw new CliError(`Unable to run ssh: ${result.error.message}`);
    if (result.status !== 0) {
      const detail = capture ? String(result.stderr || result.stdout || '').trim() : '';
      throw new CliError(`SSH exited with code ${result.status}${detail ? `\n${detail}` : ''}`);
    }
    return capture ? String(result.stdout).trim() : '';
  } finally {
    if (temporaryDirectory) rmSync(temporaryDirectory, { recursive: true, force: true });
  }
}

export function shouldForwardSshAgent(options = {}) {
  return options.forwardAgent === true;
}

function connectPreview(aws, name, options) {
  callerIdentity(aws);
  const instance = resolveManagedInstance(aws, name);
  runSsh(aws, instance, options, [], { forwardAgent: shouldForwardSshAgent(options) });
}

function extendPreview(aws, name, duration, options) {
  callerIdentity(aws);
  const instance = resolveManagedInstance(aws, name);
  const expiration = extendExpiration(findTag(instance, 'ExpiresAt'), duration);
  runSsh(aws, instance, options, [
    `sudo shutdown -c || true; sudo shutdown -h +${minutesUntil(expiration)}`,
  ], { capture: true });
  aws.run([
    'ec2', 'create-tags',
    '--resources', ...workspaceResourceIds(instance),
    '--tags', JSON.stringify([{ Key: 'ExpiresAt', Value: expiration }]),
  ]);
  console.log(`Expires: ${expiration}`);
}

async function confirm(message) {
  if (!process.stdin.isTTY) throw new CliError(`${message} Re-run with --yes.`);
  const prompt = createInterface({ input: process.stdin, output: process.stdout });
  try {
    return /^y(?:es)?$/i.test((await prompt.question(`${message} [y/N] `)).trim());
  } finally {
    prompt.close();
  }
}

export async function terminatePreview(aws, name, options) {
  callerIdentity(aws);
  const instance = resolveManagedInstance(aws, name);
  const hostname = findTag(instance, 'Hostname');
  if (!options.yes && !await confirm(`Terminate ${hostname || instance.InstanceId}?`)) {
    console.log('Cancelled.');
    return;
  }

  terminateInstance(aws, instance, hostname);
  console.log(`Terminated ${instance.InstanceId}.`);
}

function requirePositionals(command, values, count, usage = command) {
  if (values.length !== count) throw new CliError(`Usage: preview.mjs ${usage}`);
}

export async function main(argv = process.argv.slice(2)) {
  const { positionals, options } = parseCliArgs(argv);
  const command = positionals.shift();
  if (!command || options.help) {
    console.log(HELP);
    return;
  }
  const aws = new AwsCli();
  switch (command) {
    case 'create':
      allowOptions(options, ['owner', 'ttl']);
      requirePositionals(command, positionals, 1, 'create <issue> [--owner <name>] [--ttl <duration>]');
      await createPreview(aws, positionals[0], options);
      break;
    case 'list':
      allowOptions(options, []);
      requirePositionals(command, positionals, 0);
      listPreviews(aws);
      break;
    case 'connect':
      allowOptions(options, ['identity', 'forwardAgent']);
      requirePositionals(command, positionals, 1, 'connect <name> [--identity <key>] [--forward-agent]');
      connectPreview(aws, positionals[0], options);
      break;
    case 'extend':
      allowOptions(options, ['identity']);
      requirePositionals(command, positionals, 2, 'extend <name> <duration> [--identity <key>]');
      extendPreview(aws, positionals[0], positionals[1], options);
      break;
    case 'terminate':
      allowOptions(options, ['yes']);
      requirePositionals(command, positionals, 1, 'terminate <name> [--yes]');
      await terminatePreview(aws, positionals[0], options);
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
