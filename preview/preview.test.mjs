import assert from 'node:assert/strict';
import test from 'node:test';

import { CONFIG } from './config.mjs';
import {
  buildHostname,
  expiresAt,
  extendExpiration,
  minutesUntil,
  normalizeDnsLabel,
  parseDuration,
} from './lib.mjs';
import { waitForPreview } from './preview.mjs';

const now = new Date('2026-09-01T00:00:00.000Z');
const hostname = `dur-5542.vitalii.${CONFIG.domain}`;

test('names and deadlines', () => {
  assert.equal(buildHostname('DUR-5542', 'Vitalii@getduranta.com'), hostname);
  assert.equal(normalizeDnsLabel('A'.repeat(90)).length, 63);
  assert.equal(expiresAt('48h', now), '2026-09-03T00:00:00.000Z');
  // An expired tag extends from now, not from the past deadline.
  assert.equal(extendExpiration('2026-08-31T00:00:00.000Z', '12h', now), '2026-09-01T12:00:00.000Z');
  assert.equal(minutesUntil('2026-09-01T12:00:30.000Z', now), 721);
  assert.equal(minutesUntil('2026-08-31T00:00:00.000Z', now), 1);
  assert.throws(() => parseDuration('48'), /Invalid duration/);
});

test('readiness tolerates connection errors and 502 until the deadline', async () => {
  let clock = 0;
  const responses = [
    () => { throw new Error('ECONNREFUSED'); },
    () => new Response('', { status: 502 }),
    () => new Response('', { status: 200 }),
  ];
  const timing = { now: () => clock, sleep: async (delayMs) => { clock += delayMs; } };
  await waitForPreview(hostname, 30000, { ...timing, fetchImpl: async () => responses.shift()() });
  await assert.rejects(
    waitForPreview(hostname, 15000, { ...timing, fetchImpl: async () => new Response('', { status: 502 }) }),
    /public app returned HTTP 502/,
  );
});
