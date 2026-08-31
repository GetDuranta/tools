import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import {
  assertExpectedAccount,
  buildAmiName,
  buildBuilderArgs,
  cleanupUnpublishedImage,
  parseArgs,
  selectPruneCandidates,
} from './bake.mjs';

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
  const args = buildBuilderArgs('ami-base', '/dev/sda1', 'token');
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
