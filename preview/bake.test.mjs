import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import { buildAmiName, parseArgs, selectPruneCandidates, validateCpuInstanceType } from './bake.mjs';

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
