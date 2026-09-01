import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import {
  assertPreviewAccount,
  assertVolumeInitializationRateSupport,
  buildAuditTags,
  buildBootstrapUserData,
  buildCreditSpecificationArgs,
  buildHostname,
  buildRunInstancesArgs,
  buildTags,
  countManagedInstancesForCreator,
  expiresAt,
  extendExpiration,
  normalizeDnsLabel,
  parseDuration,
  tagsToAws,
  validateGoldenAmi,
  workspaceResourceIds,
} from './lib.mjs';
import { terminatePreview, waitForPreview } from './preview.mjs';

const identity = {
  Account: CONFIG.accountId,
  Arn: `arn:aws:sts::${CONFIG.accountId}:assumed-role/Preview/vitalii@getduranta.com`,
  UserId: 'AROAEXAMPLE:vitalii@getduranta.com',
};
const hostname = `dur-5542.vitalii.${CONFIG.domain}`;
const createdAt = '2026-09-01T00:00:00.000Z';
const expiration = '2026-09-03T00:00:00.000Z';
const tags = buildTags({
  auditTags: buildAuditTags(identity, createdAt),
  issue: 'DUR-5542',
  owner: 'vitalii',
  hostname,
  expiration,
});

test('builds stable Preview names and deadlines', () => {
  const now = new Date(createdAt);
  assert.equal(buildHostname('DUR-5542', 'Vitalii@getduranta.com'), hostname);
  assert.equal(normalizeDnsLabel('A'.repeat(90)).length, 63);
  assert.equal(expiresAt('48h', now), expiration);
  assert.equal(
    extendExpiration('2026-09-02T00:00:00.000Z', '12h', now),
    '2026-09-02T12:00:00.000Z',
  );
  assert.throws(() => parseDuration('48'), /Invalid duration/);
});

test('pins the ARM64 T4g infrastructure contract', () => {
  assert.deepEqual({
    architecture: CONFIG.architecture,
    baseAmiParameter: CONFIG.baseAmiParameter,
    builderInstanceType: CONFIG.builderInstanceType,
    goldenAmiParameter: CONFIG.goldenAmiParameter,
    volumeInitializationRate: CONFIG.volumeInitializationRate,
    volumeSize: CONFIG.volumeSize,
    workspaceInstanceType: CONFIG.workspaceInstanceType,
  }, {
    architecture: 'arm64',
    baseAmiParameter: '/aws/service/canonical/ubuntu/server/26.04/stable/current/arm64/hvm/ebs-gp3/ami-id',
    builderInstanceType: 't4g.2xlarge',
    goldenAmiParameter: '/duranta-preview/golden-ami-id-arm64',
    volumeInitializationRate: 300,
    volumeSize: 100,
    workspaceInstanceType: 't4g.2xlarge',
  });
  assert.deepEqual(
    buildCreditSpecificationArgs('t4g.2xlarge'),
    ['--credit-specification', 'CpuCredits=unlimited'],
  );
  assert.deepEqual(buildCreditSpecificationArgs('m7i.4xlarge'), []);
});

test('requires an AWS CLI model that supports provisioned volume initialization', () => {
  const supported = {
    BlockDeviceMappings: [{ Ebs: { VolumeInitializationRate: 0 } }],
  };
  assert.equal(assertVolumeInitializationRateSupport(supported), supported);
  assert.throws(
    () => assertVolumeInitializationRateSupport({ BlockDeviceMappings: [{ Ebs: {} }] }),
    /update AWS CLI v2/,
  );
});

test('waits for the exact marker and then verifies the public app', async () => {
  let now = 0;
  const responses = [
    new Response('<html>SPA fallback</html>', { status: 200 }),
    new Response('ready\n', { status: 200 }),
    new Response('temporarily unavailable', { status: 502 }),
    new Response('ready\n', { status: 200 }),
    new Response('<html>app</html>', { status: 200 }),
  ];
  const urls = [];

  await waitForPreview(hostname, 30000, {
    fetchImpl: async (url) => {
      urls.push(url);
      return responses.shift();
    },
    now: () => now,
    sleep: async (delayMs) => { now += delayMs; },
  });

  assert.deepEqual(urls, [
    `https://${hostname}/__preview/ready`,
    `https://${hostname}/__preview/ready`,
    `https://${hostname}/a/`,
    `https://${hostname}/__preview/ready`,
    `https://${hostname}/a/`,
  ]);
});

test('account and creator identity scope mutations and limits', () => {
  assert.equal(assertPreviewAccount(identity), identity);
  assert.throws(
    () => assertPreviewAccount({ ...identity, Account: '111111111111' }),
    new RegExp(`expected Preview account ${CONFIG.accountId}`),
  );

  const instance = (instanceTags) => ({ Tags: tagsToAws(instanceTags) });
  assert.equal(countManagedInstancesForCreator([
    instance(tags),
    instance({ ...tags, CreatorId: 'AROAOTHER:alex@getduranta.com' }),
    instance({ ...tags, ManagedBy: 'other' }),
  ], identity.UserId), 1);
});

test('accepts only golden AMIs for the configured architecture', () => {
  const image = {
    Architecture: CONFIG.architecture,
    ImageId: 'ami-golden',
    OwnerId: CONFIG.accountId,
    RootDeviceName: '/dev/sda1',
    State: 'available',
    Tags: tagsToAws({ ManagedBy: CONFIG.managedBy, Purpose: 'golden' }),
  };
  assert.equal(validateGoldenAmi(image, CONFIG.accountId), image);
  assert.throws(
    () => validateGoldenAmi({ ...image, Architecture: 'x86_64' }, CONFIG.accountId),
    /arm64 Preview golden AMI/,
  );
});

test('launches a tagged disposable workspace', () => {
  const args = buildRunInstancesArgs({
    amiId: 'ami-123',
    clientToken: 'token',
    rootDeviceName: '/dev/sda1',
    tags,
    userData: buildBootstrapUserData({ hostname, expiration }),
  });
  assert.equal(
    args[args.indexOf('--instance-type') + 1],
    CONFIG.workspaceInstanceType,
  );
  assert.equal(
    args[args.indexOf('--credit-specification') + 1],
    'CpuCredits=unlimited',
  );
  assert.equal(args[args.indexOf('--instance-initiated-shutdown-behavior') + 1], 'terminate');
  assert.ok(args.includes('--disable-api-stop'));

  const network = JSON.parse(args[args.indexOf('--network-interfaces') + 1])[0];
  const storage = JSON.parse(args[args.indexOf('--block-device-mappings') + 1])[0].Ebs;
  assert.equal(network.AssociatePublicIpAddress, true);
  assert.equal(network.DeleteOnTermination, true);
  assert.equal(storage.DeleteOnTermination, true);
  assert.equal(storage.VolumeInitializationRate, 300);
  assert.equal(storage.VolumeSize, 100);

  const specifications = JSON.parse(args[args.indexOf('--tag-specifications') + 1]);
  assert.deepEqual(
    specifications.map(({ ResourceType }) => ResourceType),
    ['instance', 'volume', 'network-interface'],
  );
  for (const specification of specifications) {
    const resourceTags = Object.fromEntries(
      specification.Tags.map(({ Key, Value }) => [Key, Value]),
    );
    assert.equal(resourceTags.ManagedBy, CONFIG.managedBy);
    assert.equal(resourceTags.Purpose, 'workspace');
    assert.equal(resourceTags.CreatorId, identity.UserId);
    assert.equal(resourceTags.CreatedBy, 'vitalii@getduranta.com');
    assert.equal(resourceTags.CreatedAt, createdAt);
    assert.equal(resourceTags.ExpiresAt, expiration);
  }
  assert.deepEqual(workspaceResourceIds({
    InstanceId: 'i-123',
    BlockDeviceMappings: [{ Ebs: { VolumeId: 'vol-123' } }],
    NetworkInterfaces: [{ NetworkInterfaceId: 'eni-123' }],
  }), ['i-123', 'vol-123', 'eni-123']);

  const userData = args[args.indexOf('--user-data') + 1];
  assert.match(userData, /duranta-preview-bootstrap/);
  assert.match(userData, /shutdown -h now/);
});

test('termination still waits when exact DNS cleanup fails', async () => {
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
