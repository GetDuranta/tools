import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import { buildAuditTags } from './lib.mjs';
import {
  assertExpectedAccount,
  buildBuilderArgs,
  buildAmiName,
  buildCreateImageArgs,
  cleanupUnpublishedImage,
  managedImagesForArchitecture,
  selectPruneCandidates,
  validateBaseAmi,
  waitForImageAvailable,
} from './bake.mjs';

const identity = {
  Account: CONFIG.accountId,
  Arn: `arn:aws:sts::${CONFIG.accountId}:assumed-role/Preview/vitalii@getduranta.com`,
  UserId: 'AROAEXAMPLE:vitalii@getduranta.com',
};
const createdAt = '2026-09-01T03:04:05.000Z';
const auditTags = buildAuditTags(identity, createdAt);

test('AMI names are scoped to the configured architecture', () => {
  assert.match(buildAmiName(new Date(createdAt), 'abcdef12'), /duranta-preview-arm64-main/);
});

test('rejects base AMIs and retention candidates from another architecture', () => {
  const armBaseAmi = { Architecture: 'arm64', RootDeviceName: '/dev/sda1' };
  assert.equal(validateBaseAmi(armBaseAmi, 'ami-arm'), armBaseAmi);
  assert.throws(
    () => validateBaseAmi({ Architecture: 'x86_64', RootDeviceName: '/dev/sda1' }, 'ami-x86'),
    /expected arm64/,
  );

  const managedTags = [
    { Key: 'ManagedBy', Value: CONFIG.managedBy },
    { Key: 'Purpose', Value: 'golden' },
  ];
  assert.deepEqual(
    managedImagesForArchitecture([
      { ImageId: 'ami-arm', Architecture: 'arm64', Tags: managedTags },
      { ImageId: 'ami-x86', Architecture: 'x86_64', Tags: managedTags },
    ]).map(({ ImageId }) => ImageId),
    ['ami-arm'],
  );
});

test('bake creates disposable audited resources only in the Preview account', () => {
  assert.equal(assertExpectedAccount(identity), identity);
  assert.throws(
    () => assertExpectedAccount({ Account: '000000000000' }),
    /not the Duranta Preview account/,
  );

  const builderArgs = buildBuilderArgs('ami-base', '/dev/sda1', 'token', auditTags);
  assert.equal(
    builderArgs[builderArgs.indexOf('--instance-type') + 1],
    CONFIG.builderInstanceType,
  );
  assert.equal(
    builderArgs[builderArgs.indexOf('--credit-specification') + 1],
    'CpuCredits=unlimited',
  );
  assert.equal(
    builderArgs[builderArgs.indexOf('--instance-initiated-shutdown-behavior') + 1],
    'terminate',
  );
  assert.ok(builderArgs.includes('--disable-api-stop'));
  assert.match(builderArgs[builderArgs.indexOf('--user-data') + 1], /--on-active=6h/);
  assert.equal(
    JSON.parse(builderArgs[builderArgs.indexOf('--network-interfaces') + 1])[0].DeleteOnTermination,
    true,
  );
  assert.equal(
    JSON.parse(builderArgs[builderArgs.indexOf('--block-device-mappings') + 1])[0].Ebs.DeleteOnTermination,
    true,
  );
  assert.equal(
    JSON.parse(builderArgs[builderArgs.indexOf('--block-device-mappings') + 1])[0].Ebs.VolumeSize,
    100,
  );

  const builderTags = JSON.parse(builderArgs[builderArgs.indexOf('--tag-specifications') + 1]);
  assert.deepEqual(
    builderTags.map(({ ResourceType }) => ResourceType),
    ['instance', 'volume', 'network-interface'],
  );
  for (const specification of builderTags) {
    const tags = Object.fromEntries(specification.Tags.map(({ Key, Value }) => [Key, Value]));
    assert.equal(tags.ManagedBy, CONFIG.managedBy);
    assert.equal(tags.Purpose, 'ami-builder');
    assert.equal(tags.CreatorId, identity.UserId);
    assert.equal(tags.CreatedBy, 'vitalii@getduranta.com');
    assert.equal(tags.CreatedAt, createdAt);
  }

  const imageArgs = buildCreateImageArgs('i-builder', 'duranta-preview-main', 'abcdef12', auditTags);
  const imageTags = JSON.parse(imageArgs[imageArgs.indexOf('--tag-specifications') + 1]);
  assert.deepEqual(imageTags.map(({ ResourceType }) => ResourceType), ['image', 'snapshot']);
  for (const specification of imageTags) {
    const tags = Object.fromEntries(specification.Tags.map(({ Key, Value }) => [Key, Value]));
    assert.equal(tags.ManagedBy, CONFIG.managedBy);
    assert.equal(tags.Purpose, 'golden');
    assert.equal(tags.Architecture, CONFIG.architecture);
    assert.equal(tags.CreatorId, identity.UserId);
    assert.equal(tags.CreatedBy, 'vitalii@getduranta.com');
    assert.equal(tags.CreatedAt, createdAt);
  }
});

test('AMI retention protects the published image and deletes only an unpublished image', async () => {
  const images = [
    { ImageId: 'ami-new', CreationDate: '2026-08-28T03:00:00Z' },
    { ImageId: 'ami-recent', CreationDate: '2026-08-27T03:00:00Z' },
    { ImageId: 'ami-published', CreationDate: '2026-08-26T03:00:00Z' },
    { ImageId: 'ami-old', CreationDate: '2026-08-25T03:00:00Z' },
  ];
  assert.deepEqual(
    selectPruneCandidates(images, 'ami-published').map(({ ImageId }) => ImageId),
    ['ami-old'],
  );

  const calls = [];
  const result = await cleanupUnpublishedImage('ami-created', {
    aws: async (args) => {
      calls.push(args);
      if (args[0] === 'ssm') return { Parameter: { Value: 'ami-published' } };
      if (args[1] === 'describe-images') return { Images: [{ ImageId: 'ami-created' }] };
      return {};
    },
    warn: () => {},
  });
  assert.equal(result, true);
  assert.deepEqual(calls.at(-1), [
    'ec2', 'deregister-image',
    '--image-id', 'ami-created',
    '--delete-associated-snapshots',
  ]);
});

test('AMI availability polling outlives the short AWS CLI waiter', async () => {
  const states = ['pending', 'pending', 'available'];
  const sleeps = [];
  const image = await waitForImageAvailable('ami-created', {
    aws: async () => ({
      Images: [{ ImageId: 'ami-created', State: states.shift() }],
    }),
    sleep: async (delayMs) => sleeps.push(delayMs),
    intervalMs: 25,
    maxAttempts: 4,
  });

  assert.equal(image.State, 'available');
  assert.deepEqual(sleeps, [25, 25]);
});

test('AMI availability polling fails fast and reports timeouts', async () => {
  await assert.rejects(
    waitForImageAvailable('ami-failed', {
      aws: async () => ({ Images: [{ ImageId: 'ami-failed', State: 'failed' }] }),
      sleep: async () => {},
      maxAttempts: 3,
    }),
    /terminal state: failed/,
  );

  await assert.rejects(
    waitForImageAvailable('ami-pending', {
      aws: async () => ({ Images: [{ ImageId: 'ami-pending', State: 'pending' }] }),
      sleep: async () => {},
      maxAttempts: 3,
    }),
    /not available after 3 attempts.*pending/,
  );
});

test('AMI availability polling retries eventual consistency and rejects other terminal states', async () => {
  let attempts = 0;
  const image = await waitForImageAvailable('ami-created', {
    aws: async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('InvalidAMIID.NotFound: The image id does not exist');
      return { Images: [{ ImageId: 'ami-created', State: 'available' }] };
    },
    sleep: async () => {},
    maxAttempts: 3,
  });
  assert.equal(image.State, 'available');
  assert.equal(attempts, 2);

  await assert.rejects(
    waitForImageAvailable('ami-invalid', {
      aws: async () => ({ Images: [{ ImageId: 'ami-invalid', State: 'invalid' }] }),
      sleep: async () => {},
      maxAttempts: 3,
    }),
    /terminal state: invalid/,
  );

  await assert.rejects(
    waitForImageAvailable('ami-created', {
      aws: async () => { throw new Error('AccessDenied: nope'); },
      sleep: async () => {},
      maxAttempts: 3,
    }),
    /AccessDenied/,
  );
});

test('golden warm-up removes stateful service volumes', () => {
  const provision = readFileSync(new URL('./remote/provision.sh', import.meta.url), 'utf8');
  assert.match(provision, /podman volume rm[\s\\]+duranta-preview_db_data/);
  assert.match(provision, /duranta-preview_clickhouse_data/);
  assert.match(provision, /duranta-preview_blob_data/);
});
