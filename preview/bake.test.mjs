import assert from 'node:assert/strict';
import test from 'node:test';

import { cleanupUnpublishedImage, selectPruneCandidates, waitForImageAvailable } from './bake.mjs';

test('retention keeps two images and never the published one; cleanup deregisters only an unpublished image', async () => {
  const images = [
    { ImageId: 'ami-new', CreationDate: '2026-08-28T03:00:00Z' },
    { ImageId: 'ami-recent', CreationDate: '2026-08-27T03:00:00Z' },
    { ImageId: 'ami-published', CreationDate: '2026-08-26T03:00:00Z' },
    { ImageId: 'ami-old', CreationDate: '2026-08-25T03:00:00Z' },
  ];
  assert.deepEqual(selectPruneCandidates(images, 'ami-published').map(({ ImageId }) => ImageId), ['ami-old']);

  const aws = async (args) => {
    if (args[0] === 'ssm') return { Parameter: { Value: 'ami-published' } };
    if (args[1] === 'describe-images') return { Images: [{ ImageId: args[3], State: 'available' }] };
    return {};
  };
  assert.equal(await cleanupUnpublishedImage('ami-published', { aws, warn() {} }), false);
  assert.equal(await cleanupUnpublishedImage('ami-created', { aws, warn() {} }), true);
});

test('AMI polling retries eventual consistency and stops on a terminal state', async () => {
  let attempts = 0;
  const image = await waitForImageAvailable('ami-created', {
    aws: async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('InvalidAMIID.NotFound: The image id does not exist');
      return { Images: [{ ImageId: 'ami-created', State: attempts < 3 ? 'pending' : 'available' }] };
    },
    sleep: async () => {},
    maxAttempts: 4,
  });
  assert.equal(image.State, 'available');
  await assert.rejects(
    waitForImageAvailable('ami-failed', {
      aws: async () => ({ Images: [{ ImageId: 'ami-failed', State: 'failed' }] }),
      sleep: async () => {},
      maxAttempts: 3,
    }),
    /terminal state: failed/,
  );
});
