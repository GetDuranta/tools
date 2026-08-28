import assert from 'node:assert/strict';
import test from 'node:test';

import { extendDeadline, parseDuration } from './ttl.mjs';

test('TTL duration accepts CLI seconds and human units', () => {
  assert.equal(parseDuration('86400s'), 86400);
  assert.equal(parseDuration('1d12h'), 129600);
});

test('TTL rejects empty, zero, and malformed durations', () => {
  for (const input of ['', '0s', '-1h', '1hour']) {
    assert.throws(() => parseDuration(input), /Invalid duration/);
  }
});

test('TTL extends from the later of now and current deadline', () => {
  assert.equal(
    extendDeadline('2026-08-28T10:00:00Z', '2h', new Date('2026-08-28T09:00:00Z')),
    '2026-08-28T12:00:00.000Z',
  );
  assert.equal(
    extendDeadline('2026-08-28T08:00:00Z', '2h', new Date('2026-08-28T09:00:00Z')),
    '2026-08-28T11:00:00.000Z',
  );
});
