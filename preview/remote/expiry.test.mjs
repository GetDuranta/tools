import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { access, chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import test from 'node:test';

const execFileAsync = promisify(execFile);
const helper = fileURLToPath(new URL('./expiry.mjs', import.meta.url));

async function fixture() {
  const directory = await mkdtemp(join(tmpdir(), 'preview-expiry-'));
  const deadline = join(directory, 'deadline');
  const marker = join(directory, 'shutdown-called');
  const shutdown = join(directory, 'shutdown');
  await writeFile(shutdown, '#!/bin/sh\nprintf called >"$DURANTA_PREVIEW_SHUTDOWN_MARKER"\n');
  await chmod(shutdown, 0o755);
  return {
    deadline,
    directory,
    marker,
    env: {
      ...process.env,
      DURANTA_PREVIEW_DEADLINE: deadline,
      DURANTA_PREVIEW_SHUTDOWN_MARKER: marker,
      PATH: `${directory}:${process.env.PATH}`,
    },
  };
}

test('expiry keeps a future host alive and shuts down an expired host', async (context) => {
  const future = await fixture();
  const expired = await fixture();
  context.after(() => Promise.all([
    rm(future.directory, { recursive: true, force: true }),
    rm(expired.directory, { recursive: true, force: true }),
  ]));

  const deadline = new Date(Date.now() + 60 * 60 * 1000).toISOString();
  await execFileAsync(process.execPath, [helper, 'set', deadline], { env: future.env });
  await execFileAsync(process.execPath, [helper], { env: future.env });
  assert.equal((await readFile(future.deadline, 'utf8')).trim(), deadline);
  await assert.rejects(access(future.marker));

  await writeFile(expired.deadline, '2000-01-01T00:00:00.000Z\n');
  await execFileAsync(process.execPath, [helper], { env: expired.env });
  assert.equal(await readFile(expired.marker, 'utf8'), 'called');
});
