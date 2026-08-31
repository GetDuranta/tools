import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import { buildAuditTags } from './lib.mjs';
import {
  assertExpectedAccount,
  buildAmiName,
  buildBuilderArgs,
  buildCreateImageArgs,
  cleanupUnpublishedImage,
  parseArgs,
  selectPruneCandidates,
} from './bake.mjs';

const identity = {
  Account: CONFIG.accountId,
  Arn: `arn:aws:sts::${CONFIG.accountId}:assumed-role/Preview/vitalii@getduranta.com`,
  UserId: 'AROAEXAMPLE:vitalii@getduranta.com',
};
const createdAt = '2026-09-01T03:04:05.000Z';
const auditTags = buildAuditTags(identity, createdAt);

test('bake accepts only its fixed command and SSH identity', () => {
  assert.deepEqual(parseArgs([]), { command: null, help: false, identity: null });
  assert.equal(parseArgs(['bake', '--identity', './key']).identity.endsWith('/key'), true);
  assert.equal(parseArgs(['--help']).help, true);
  assert.throws(() => parseArgs(['list']), /Unknown command/);
  assert.throws(() => parseArgs(['bake', '--region', 'us-east-1']), /Unknown option/);
});

test('bake requires the exact Preview account', () => {
  assert.equal(assertExpectedAccount({ Account: CONFIG.accountId }).Account, CONFIG.accountId);
  assert.throws(() => assertExpectedAccount({ Account: '000000000000' }), /not the Duranta Preview account/);
});

test('builder settings make compute and storage disposable', () => {
  const args = buildBuilderArgs('ami-base', '/dev/sda1', 'token', auditTags);
  assert.equal(args[args.indexOf('--instance-type') + 1], CONFIG.instanceType);
  assert.equal(args[args.indexOf('--instance-initiated-shutdown-behavior') + 1], 'terminate');
  assert.ok(args.includes('--disable-api-stop'));
  assert.match(args[args.indexOf('--user-data') + 1], /--on-active=6h/);

  const network = JSON.parse(args[args.indexOf('--network-interfaces') + 1]);
  assert.equal(network[0].DeleteOnTermination, true);
  assert.equal(network[0].SubnetId, CONFIG.subnetId);
  assert.deepEqual(network[0].Groups, [CONFIG.securityGroupId]);

  const storage = JSON.parse(args[args.indexOf('--block-device-mappings') + 1]);
  assert.equal(storage[0].Ebs.DeleteOnTermination, true);
  assert.equal(storage[0].Ebs.VolumeSize, CONFIG.volumeSize);

  const specifications = JSON.parse(args[args.indexOf('--tag-specifications') + 1]);
  assert.deepEqual(specifications.map(({ ResourceType }) => ResourceType), [
    'instance', 'volume', 'network-interface',
  ]);
  for (const specification of specifications) {
    const tags = Object.fromEntries(specification.Tags.map(({ Key, Value }) => [Key, Value]));
    assert.deepEqual(tags, {
      Name: 'duranta-preview-ami-builder',
      ManagedBy: CONFIG.managedBy,
      Purpose: 'ami-builder',
      ...auditTags,
    });
  }
});

test('golden AMI and snapshot retain the bake audit identity', () => {
  const args = buildCreateImageArgs('i-builder', 'duranta-preview-main', 'abcdef12', auditTags);
  const specifications = JSON.parse(args[args.indexOf('--tag-specifications') + 1]);
  assert.deepEqual(specifications.map(({ ResourceType }) => ResourceType), ['image', 'snapshot']);
  for (const specification of specifications) {
    const tags = Object.fromEntries(specification.Tags.map(({ Key, Value }) => [Key, Value]));
    assert.equal(tags.CreatorId, identity.UserId);
    assert.equal(tags.CreatedBy, 'vitalii@getduranta.com');
    assert.equal(tags.CreatedAt, createdAt);
  }
});

test('AMI names contain a readable timestamp and source commit', () => {
  assert.equal(
    buildAmiName(new Date('2026-08-28T07:09:10.000Z'), 'ABCDEF1234'),
    'duranta-preview-main-20260828-0709-abcdef12',
  );
});

test('automatic prune keeps two newest images and the published pointer', () => {
  const images = [
    { ImageId: 'ami-new', CreationDate: '2026-08-28T03:00:00Z' },
    { ImageId: 'ami-current', CreationDate: '2026-08-27T03:00:00Z' },
    { ImageId: 'ami-old', CreationDate: '2026-08-26T03:00:00Z' },
    { ImageId: 'ami-older', CreationDate: '2026-08-25T03:00:00Z' },
  ];
  assert.deepEqual(
    selectPruneCandidates(images, 'ami-current').map(({ ImageId }) => ImageId),
    ['ami-old', 'ami-older'],
  );
});

test('failed publication removes only the exact created AMI and its snapshots', async () => {
  const calls = [];
  const result = await cleanupUnpublishedImage('ami-created', {
    aws: async (args) => {
      calls.push(args);
      if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
      if (args[1] === 'describe-images') return { Images: [{
        ImageId: 'ami-created',
      }] };
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
