import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import {
  buildAmiName,
  parseArgs,
  rollbackUnpublishedImage,
  selectPruneCandidates,
  validateCpuInstanceType,
} from './bake.mjs';

test('bake defaults are read-only and CPU-based', () => {
  const options = parseArgs([], {});
  assert.equal(options.command, 'bake');
  assert.equal(options.apply, false);
  assert.equal(options.instanceType, 'm7i.4xlarge');
  assert.equal(options.volumeSize, 200);
});

test('bake validates integer options', () => {
  assert.throws(() => parseArgs(['prune', '--keep', '-1'], {}), /non-negative integer/);
  assert.throws(() => parseArgs(['bake', '--volume-size', '0'], {}), /positive integer/);
});

test('bake rejects accelerated and non-Standard builders', () => {
  const cpu = {
    InstanceType: 'm7i.4xlarge',
    ProcessorInfo: { SupportedArchitectures: ['x86_64'] },
    VCpuInfo: { DefaultVCpus: 16 },
  };
  assert.equal(validateCpuInstanceType('m7i.4xlarge', cpu), 16);
  assert.throws(() => validateCpuInstanceType('g5.4xlarge', {
    ...cpu,
    InstanceType: 'g5.4xlarge',
    GpuInfo: { Gpus: [{ Name: 'A10G' }] },
  }), /CPU-only/);
  assert.throws(() => validateCpuInstanceType('p5.48xlarge', {
    ...cpu,
    InstanceType: 'p5.48xlarge',
  }), /Standard On-Demand/);
});

test('builder blocks API stop and removes protection before termination', async () => {
  const source = await readFile(new URL('./bake.mjs', import.meta.url), 'utf8');
  assert.match(source, /'--disable-api-stop'/);
  assert.match(source, /'--count', '1'/);
  assert.doesNotMatch(source, /'--min-count'|'--max-count'/);
  const unprotect = source.indexOf("'modify-instance-attribute', '--instance-id', instanceId, '--no-disable-api-stop'");
  const terminate = source.indexOf("'terminate-instances', '--instance-ids', instanceId");
  assert.ok(unprotect >= 0 && terminate > unprotect);
});

test('bake publishes before committing the AMI and rolls back only unpublished images', async () => {
  const source = await readFile(new URL('./bake.mjs', import.meta.url), 'utf8');
  const publish = source.indexOf("'ssm', 'put-parameter'");
  const commit = source.indexOf('imagePublished = true;', publish);
  const rollback = source.indexOf('if (imageId && !imagePublished)');
  assert.ok(publish >= 0 && commit > publish && rollback > commit);
});

test('AMI names contain a readable timestamp and source commit', () => {
  assert.equal(
    buildAmiName(new Date('2026-08-28T07:09:10.000Z'), 'ABCDEF1234'),
    'duranta-preview-main-20260828-0709-abcdef12',
  );
});

test('prune keeps newest images and the published pointer', () => {
  const images = [
    { ImageId: 'ami-new', CreationDate: '2026-08-28T03:00:00Z' },
    { ImageId: 'ami-current', CreationDate: '2026-08-27T03:00:00Z' },
    { ImageId: 'ami-old', CreationDate: '2026-08-26T03:00:00Z' },
  ];
  assert.deepEqual(
    selectPruneCandidates(images, 1, 'ami-current').map(({ ImageId }) => ImageId),
    ['ami-old'],
  );
});

test('failed bake removes only its exact owned AMI and snapshots after references settle', async () => {
  const calls = [];
  let imageLookups = 0;
  let referenceLookups = 0;
  let deleteAttempts = 0;
  const aws = async (_config, args) => {
    calls.push(args);
    if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
    if (args[1] === 'describe-images' && args.includes('--image-ids')) {
      imageLookups += 1;
      if (imageLookups === 1) return { Images: [] };
      return { Images: [{
        ImageId: 'ami-created',
        OwnerId: '123456789012',
        State: 'available',
        Tags: [
          { Key: 'ManagedBy', Value: 'duranta-preview' },
          { Key: 'Purpose', Value: 'golden' },
        ],
        BlockDeviceMappings: [{ Ebs: { SnapshotId: 'snap-created' } }],
      }] };
    }
    if (args[1] === 'describe-snapshots') return { Snapshots: [{
      SnapshotId: 'snap-created',
      OwnerId: '123456789012',
      Tags: [
        { Key: 'ManagedBy', Value: 'duranta-preview' },
        { Key: 'Purpose', Value: 'golden' },
      ],
    }] };
    if (args[1] === 'deregister-image') return {};
    if (args[1] === 'describe-images') {
      referenceLookups += 1;
      return { Images: referenceLookups === 1 ? [{ ImageId: 'ami-created' }] : [] };
    }
    if (args[1] === 'delete-snapshot') {
      deleteAttempts += 1;
      if (deleteAttempts === 1) throw new Error('InvalidSnapshot.InUse');
      return {};
    }
    throw new Error(`Unexpected call: ${args.join(' ')}`);
  };

  const result = await rollbackUnpublishedImage(
    { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
    'ami-created',
    '123456789012',
    { aws, attempts: 4, delay: async () => {} },
  );

  assert.deepEqual(result, { published: false, snapshots: ['snap-created'] });
  assert.equal(imageLookups, 2);
  assert.equal(referenceLookups, 3);
  assert.equal(deleteAttempts, 2);
  assert.deepEqual(
    calls.filter((args) => ['deregister-image', 'delete-snapshot'].includes(args[1])),
    [
      ['ec2', 'deregister-image', '--image-id', 'ami-created'],
      ['ec2', 'delete-snapshot', '--snapshot-id', 'snap-created'],
      ['ec2', 'delete-snapshot', '--snapshot-id', 'snap-created'],
    ],
  );
});

test('failed bake keeps an AMI that reached the golden pointer', async () => {
  const calls = [];
  const result = await rollbackUnpublishedImage(
    { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
    'ami-created',
    '123456789012',
    {
      aws: async (_config, args) => {
        calls.push(args);
        return { Parameter: { Value: 'ami-created' } };
      },
    },
  );

  assert.deepEqual(result, { published: true, snapshots: [] });
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], 'ssm');
});

test('rollback retries same-account resources until ownership tags are visible', async () => {
  let imageLookups = 0;
  let snapshotLookups = 0;
  let delays = 0;
  const managedTags = [
    { Key: 'ManagedBy', Value: 'duranta-preview' },
    { Key: 'Purpose', Value: 'golden' },
  ];
  await rollbackUnpublishedImage(
    { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
    'ami-created',
    '123456789012',
    {
      aws: async (_config, args) => {
        if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
        if (args[1] === 'describe-images' && args.includes('--image-ids')) {
          imageLookups += 1;
          return { Images: [{
            ImageId: 'ami-created',
            OwnerId: '123456789012',
            State: 'available',
            Tags: imageLookups === 1 ? [] : managedTags,
            BlockDeviceMappings: [{ Ebs: { SnapshotId: 'snap-created' } }],
          }] };
        }
        if (args[1] === 'describe-snapshots') {
          snapshotLookups += 1;
          return { Snapshots: [{
            SnapshotId: 'snap-created',
            OwnerId: '123456789012',
            Tags: snapshotLookups === 1 ? managedTags.slice(0, 1) : managedTags,
          }] };
        }
        if (args[1] === 'describe-images') return { Images: [] };
        if (args[1] === 'deregister-image' || args[1] === 'delete-snapshot') return {};
        throw new Error(`Unexpected call: ${args.join(' ')}`);
      },
      attempts: 3,
      delay: async () => { delays += 1; },
    },
  );

  assert.equal(imageLookups, 2);
  assert.equal(snapshotLookups, 2);
  assert.equal(delays, 2);
});

test('rollback treats missing resources as cleaned after ambiguous mutation failures', async () => {
  let deregisterAttempts = 0;
  let deleteAttempts = 0;
  let delays = 0;
  const tags = [
    { Key: 'ManagedBy', Value: 'duranta-preview' },
    { Key: 'Purpose', Value: 'golden' },
  ];
  const result = await rollbackUnpublishedImage(
    { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
    'ami-created',
    '123456789012',
    {
      aws: async (_config, args) => {
        if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
        if (args[1] === 'describe-images' && args.includes('--image-ids')) return { Images: [{
          ImageId: 'ami-created',
          OwnerId: '123456789012',
          State: 'available',
          Tags: tags,
          BlockDeviceMappings: [{ Ebs: { SnapshotId: 'snap-created' } }],
        }] };
        if (args[1] === 'describe-snapshots') return { Snapshots: [{
          SnapshotId: 'snap-created', OwnerId: '123456789012', Tags: tags,
        }] };
        if (args[1] === 'describe-images') return { Images: [] };
        if (args[1] === 'deregister-image') {
          deregisterAttempts += 1;
          throw new Error(deregisterAttempts === 1 ? 'request timed out' : 'InvalidAMIID.NotFound');
        }
        if (args[1] === 'delete-snapshot') {
          deleteAttempts += 1;
          throw new Error(deleteAttempts === 1 ? 'request timed out' : 'InvalidSnapshot.NotFound');
        }
        throw new Error(`Unexpected call: ${args.join(' ')}`);
      },
      attempts: 3,
      delay: async () => { delays += 1; },
    },
  );

  assert.deepEqual(result, { published: false, snapshots: ['snap-created'] });
  assert.equal(deregisterAttempts, 2);
  assert.equal(deleteAttempts, 2);
  assert.equal(delays, 2);
});

test('failed bake refuses to remove an AMI without matching ownership', async () => {
  const mutations = [];
  await assert.rejects(
    rollbackUnpublishedImage(
      { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
      'ami-created',
      '123456789012',
      {
        aws: async (_config, args) => {
          if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
          if (args[1] === 'describe-images') return { Images: [{
            ImageId: 'ami-created',
            OwnerId: '999999999999',
            State: 'available',
            Tags: [
              { Key: 'ManagedBy', Value: 'duranta-preview' },
              { Key: 'Purpose', Value: 'golden' },
            ],
            BlockDeviceMappings: [{ Ebs: { SnapshotId: 'snap-created' } }],
          }] };
          mutations.push(args);
          return {};
        },
        attempts: 1,
      },
    ),
    /owner does not match/,
  );
  assert.deepEqual(mutations, []);
});

test('failed bake refuses to remove a same-account AMI with conflicting tags', async () => {
  const mutations = [];
  await assert.rejects(
    rollbackUnpublishedImage(
      { goldenAmiParameter: '/duranta-preview/golden-ami-id' },
      'ami-created',
      '123456789012',
      {
        aws: async (_config, args) => {
          if (args[0] === 'ssm') return { Parameter: { Value: 'ami-current' } };
          if (args[1] === 'describe-images') return { Images: [{
            ImageId: 'ami-created',
            OwnerId: '123456789012',
            State: 'available',
            Tags: [
              { Key: 'ManagedBy', Value: 'another-tool' },
              { Key: 'Purpose', Value: 'golden' },
            ],
            BlockDeviceMappings: [{ Ebs: { SnapshotId: 'snap-created' } }],
          }] };
          mutations.push(args);
          return {};
        },
        attempts: 1,
      },
    ),
    /tag ManagedBy does not match/,
  );
  assert.deepEqual(mutations, []);
});
