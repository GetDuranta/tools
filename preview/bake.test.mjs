import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import { buildAuditTags } from './lib.mjs';
import {
  assertExpectedAccount,
  buildBuilderArgs,
  buildCreateImageArgs,
  cleanupUnpublishedImage,
  selectPruneCandidates,
} from './bake.mjs';

const identity = {
  Account: CONFIG.accountId,
  Arn: `arn:aws:sts::${CONFIG.accountId}:assumed-role/Preview/vitalii@getduranta.com`,
  UserId: 'AROAEXAMPLE:vitalii@getduranta.com',
};
const createdAt = '2026-09-01T03:04:05.000Z';
const auditTags = buildAuditTags(identity, createdAt);

test('bake creates disposable audited resources only in the Preview account', () => {
  assert.equal(assertExpectedAccount(identity), identity);
  assert.throws(
    () => assertExpectedAccount({ Account: '000000000000' }),
    /not the Duranta Preview account/,
  );

  const builderArgs = buildBuilderArgs('ami-base', '/dev/sda1', 'token', auditTags);
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
