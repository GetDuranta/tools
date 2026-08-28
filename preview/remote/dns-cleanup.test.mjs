import assert from 'node:assert/strict';
import { chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const script = new URL('./dns-cleanup.sh', import.meta.url).pathname;

async function fixture(publicIp = '203.0.113.7') {
  const directory = await mkdtemp(join(tmpdir(), 'duranta-dns-cleanup-'));
  const marker = JSON.stringify({ managedBy: 'duranta-preview', instanceId: 'i-test', owner: 'vitalii' });
  const records = {
    ResourceRecordSets: [
      { Name: 'dur-1.vitalii.duranta-preview.com.', Type: 'A', TTL: 60, ResourceRecords: [{ Value: publicIp }] },
      { Name: '*.dur-1.vitalii.duranta-preview.com.', Type: 'A', TTL: 60, ResourceRecords: [{ Value: publicIp }] },
      { Name: '_duranta-preview.dur-1.vitalii.duranta-preview.com.', Type: 'TXT', TTL: 60, ResourceRecords: [{ Value: JSON.stringify(marker) }] },
    ],
  };
  await writeFile(join(directory, 'records.json'), JSON.stringify(records));
  await writeFile(join(directory, 'instance.env'), [
    'PREVIEW_HOSTNAME=dur-1.vitalii.duranta-preview.com',
    'OWNER=vitalii',
    'HOSTED_ZONE_ID=ZTEST',
    'INSTANCE_ID=i-test',
    'PUBLIC_IP=203.0.113.7',
    '',
  ].join('\n'));
  const fakeAws = join(directory, 'aws');
  await writeFile(fakeAws, `#!/usr/bin/env bash
if [[ "$1 $2" == "route53 list-resource-record-sets" ]]; then
  exec /bin/cat "$AWS_RECORDS"
fi
printf '%s\\n' "$@" >"$AWS_CAPTURE"
`);
  await chmod(fakeAws, 0o755);
  return directory;
}

test('DNS cleanup deletes the owned record set in one batch', async () => {
  const directory = await fixture();
  try {
    const capture = join(directory, 'capture');
    const result = spawnSync('bash', [script], {
      encoding: 'utf8',
      env: {
        ...process.env,
        AWS_CAPTURE: capture,
        AWS_RECORDS: join(directory, 'records.json'),
        DURANTA_PREVIEW_STATE: join(directory, 'instance.env'),
        PATH: `${directory}:${process.env.PATH}`,
      },
    });
    assert.equal(result.status, 0, result.stderr);
    const args = (await readFile(capture, 'utf8')).trim().split('\n');
    const batch = JSON.parse(args[args.indexOf('--change-batch') + 1]);
    assert.equal(batch.Changes.length, 3);
    assert.ok(batch.Changes.every(({ Action }) => Action === 'DELETE'));
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test('DNS cleanup refuses a mismatched public IP', async () => {
  const directory = await fixture('203.0.113.8');
  try {
    const capture = join(directory, 'capture');
    const result = spawnSync('bash', [script], {
      encoding: 'utf8',
      env: {
        ...process.env,
        AWS_CAPTURE: capture,
        AWS_RECORDS: join(directory, 'records.json'),
        DURANTA_PREVIEW_STATE: join(directory, 'instance.env'),
        PATH: `${directory}:${process.env.PATH}`,
      },
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /refusing cleanup/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
