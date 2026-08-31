import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import {
  assertPreviewAccount,
  buildAwsArgs,
  buildBootstrapUserData,
  buildDnsChanges,
  buildHostname,
  buildRunInstancesArgs,
  buildSshArgs,
  buildTags,
  countManagedInstancesForCreator,
  expiresAt,
  extendExpiration,
  normalizeDnsLabel,
  normalizeOwner,
  parseDuration,
  tagsToAws,
  validateGoldenAmi,
} from './lib.mjs';
import { parseCliArgs, shouldForwardSshAgent, terminatePreview } from './preview.mjs';

const identity = {
  Account: CONFIG.accountId,
  Arn: `arn:aws:sts::${CONFIG.accountId}:assumed-role/Preview/vitalii@getduranta.com`,
  UserId: 'AROAEXAMPLE:vitalii@getduranta.com',
};

const hostname = `dur-5542.vitalii.${CONFIG.domain}`;
const expiration = '2026-09-03T00:00:00.000Z';
const tags = buildTags({
  creatorId: identity.UserId,
  issue: 'DUR-5542',
  owner: 'vitalii',
  hostname,
  expiration,
});

test('normalizes names and builds the fixed Preview hostname', () => {
  assert.equal(normalizeDnsLabel(' DUR-5542 / Agent '), 'dur-5542-agent');
  assert.equal(normalizeOwner('Vitalii@getduranta.com'), 'vitalii');
  assert.equal(buildHostname('DUR-5542', 'Vitalii@getduranta.com'), hostname);
  const long = normalizeDnsLabel('A'.repeat(90));
  assert.equal(long.length, 63);
  assert.match(long, /^a+-[a-f0-9]{8}$/);
});

test('parses TTLs and extends from the later of now and the current deadline', () => {
  const now = new Date('2026-09-01T00:00:00.000Z');
  assert.equal(parseDuration('1d12h'), 129600);
  assert.equal(expiresAt('48h', now), '2026-09-03T00:00:00.000Z');
  assert.equal(extendExpiration('2026-09-02T00:00:00.000Z', '12h', now), '2026-09-02T12:00:00.000Z');
  assert.equal(extendExpiration('invalid', '1h', now), '2026-09-01T01:00:00.000Z');
  assert.throws(() => parseDuration('48'), /Invalid duration/);
  assert.throws(() => parseDuration('0h'), /Invalid duration/);
});

test('refuses every mutation outside the fixed Preview account', () => {
  assert.equal(assertPreviewAccount(identity), identity);
  assert.throws(
    () => assertPreviewAccount({ ...identity, Account: '111111111111' }),
    new RegExp(`expected Preview account ${CONFIG.accountId}`),
  );
});

test('tags resources and counts the exact AWS creator only', () => {
  assert.deepEqual(tags, {
    Name: hostname,
    ManagedBy: CONFIG.managedBy,
    CreatorId: identity.UserId,
    Issue: 'dur-5542',
    Owner: 'vitalii',
    Hostname: hostname,
    ExpiresAt: expiration,
  });
  const instance = (instanceTags) => ({ Tags: tagsToAws(instanceTags) });
  assert.equal(countManagedInstancesForCreator([
    instance(tags),
    instance({ ...tags, CreatorId: 'AROAOTHER:alex@getduranta.com' }),
    instance({ ...tags, ManagedBy: 'other' }),
  ], identity.UserId), 1);
});

test('uses only the fixed AWS profile and region', () => {
  assert.deepEqual(
    buildAwsArgs(['ec2', 'describe-instances']),
    ['--profile', CONFIG.profile, '--region', CONFIG.region, '--no-cli-pager', '--output', 'json', 'ec2', 'describe-instances'],
  );
});

test('accepts only an available account-owned tagged golden AMI', () => {
  const image = {
    ImageId: 'ami-123',
    OwnerId: CONFIG.accountId,
    RootDeviceName: '/dev/sda1',
    State: 'available',
    Tags: tagsToAws({ ManagedBy: CONFIG.managedBy, Purpose: 'golden' }),
  };
  assert.equal(validateGoldenAmi(image, CONFIG.accountId), image);
  assert.throws(() => validateGoldenAmi({ ...image, OwnerId: '111111111111' }, CONFIG.accountId), /not a Preview golden AMI/);
  assert.throws(() => validateGoldenAmi({ ...image, State: 'pending' }, CONFIG.accountId), /not available/);
});

test('launches one disposable public instance with IMDSv2 and stop protection', () => {
  const args = buildRunInstancesArgs({
    amiId: 'ami-123',
    clientToken: 'token',
    rootDeviceName: '/dev/sda1',
    tags,
    userData: '#!/bin/sh',
  });
  assert.equal(args[args.indexOf('--instance-type') + 1], CONFIG.instanceType);
  assert.equal(args[args.indexOf('--count') + 1], '1');
  assert.equal(args[args.indexOf('--instance-initiated-shutdown-behavior') + 1], 'terminate');
  assert.ok(args.includes('--disable-api-stop'));
  const network = JSON.parse(args[args.indexOf('--network-interfaces') + 1]);
  assert.deepEqual(network[0], {
    AssociatePublicIpAddress: true,
    DeleteOnTermination: true,
    DeviceIndex: 0,
    Groups: [CONFIG.securityGroupId],
    SubnetId: CONFIG.subnetId,
  });
  const metadata = JSON.parse(args[args.indexOf('--metadata-options') + 1]);
  assert.equal(metadata.HttpTokens, 'required');
  assert.equal(metadata.HttpPutResponseHopLimit, 1);
  const block = JSON.parse(args[args.indexOf('--block-device-mappings') + 1]);
  assert.equal(block[0].Ebs.DeleteOnTermination, true);
  assert.equal(block[0].Ebs.VolumeSize, CONFIG.volumeSize);
});

test('bootstrap receives only hostname and expiry and terminates on failure', () => {
  const userData = buildBootstrapUserData({ hostname, expiration });
  assert.match(userData, new RegExp(`--hostname ${hostname.replaceAll('.', '\\.')} \\\\`));
  assert.match(userData, new RegExp(`--expires-at ${expiration.replaceAll('.', '\\.')}`));
  assert.doesNotMatch(userData, /--owner|--issue|--hosted-zone-id/);
  assert.match(userData, /shutdown -h now/);
});

test('manages exactly two A records without ownership markers', () => {
  const upserts = buildDnsChanges('UPSERT', hostname, '203.0.113.7');
  assert.deepEqual(upserts.map(({ Action, ResourceRecordSet }) => ({
    action: Action,
    name: ResourceRecordSet.Name,
    type: ResourceRecordSet.Type,
    value: ResourceRecordSet.ResourceRecords[0].Value,
  })), [
    { action: 'UPSERT', name: hostname, type: 'A', value: '203.0.113.7' },
    { action: 'UPSERT', name: `*.${hostname}`, type: 'A', value: '203.0.113.7' },
  ]);
  assert.deepEqual(
    buildDnsChanges('DELETE', hostname, '203.0.113.7').map(({ Action }) => Action),
    ['DELETE', 'DELETE'],
  );
});

test('SSH always uses SSM and forwards the agent only when explicit', () => {
  const instance = { InstanceId: 'i-123' };
  const normal = buildSshArgs(instance, '/tmp/key');
  const forwarded = buildSshArgs(instance, '/tmp/key', [], { forwardAgent: true });
  assert.equal(normal[0], '-a');
  assert.equal(forwarded[0], '-A');
  assert.match(normal[normal.indexOf('-o') + 1], /ssm start-session/);
  assert.equal(shouldForwardSshAgent({}), false);
  assert.equal(shouldForwardSshAgent({ forwardAgent: true }), true);
});

test('CLI accepts only the small command option surface', () => {
  assert.deepEqual(parseCliArgs(['create', 'DUR-5542', '--owner=vitalii', '--ttl', '48h']), {
    positionals: ['create', 'DUR-5542'],
    options: { owner: 'vitalii', ttl: '48h' },
  });
  assert.deepEqual(parseCliArgs(['connect', hostname, '--forward-agent', '--identity', '/tmp/key']), {
    positionals: ['connect', hostname],
    options: { forwardAgent: true, identity: '/tmp/key' },
  });
  assert.throws(() => parseCliArgs(['list', '--profile', 'other']), /Unknown option/);
});

test('terminate checks the account, terminates, and still waits when exact DNS cleanup fails', async () => {
  const instance = {
    InstanceId: 'i-123',
    PublicIpAddress: '203.0.113.7',
    State: { Name: 'running' },
    Tags: tagsToAws(tags),
  };
  const calls = [];
  const aws = {
    run(args) {
      calls.push(args);
      if (args[0] === 'sts') return identity;
      if (args[0] === 'ec2' && args[1] === 'describe-instances') {
        return { Reservations: [{ Instances: [instance] }] };
      }
      if (args[0] === 'route53') throw new Error('simulated DNS failure');
      return {};
    },
  };
  const originalWarn = console.warn;
  console.warn = () => {};
  try {
    await terminatePreview(aws, hostname, { yes: true });
  } finally {
    console.warn = originalWarn;
  }
  assert.deepEqual(calls[0], ['sts', 'get-caller-identity']);
  assert.ok(calls.some((args) => args[0] === 'ec2' && args[1] === 'terminate-instances'));
  assert.ok(calls.some((args) => args[0] === 'ec2' && args[1] === 'wait' && args[2] === 'instance-terminated'));
});
