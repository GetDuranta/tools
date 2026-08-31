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
  const env = {
    ...process.env,
    DURANTA_PREVIEW_DEADLINE: deadline,
    DURANTA_PREVIEW_SHUTDOWN_MARKER: marker,
    PATH: `${directory}:${process.env.PATH}`,
  };
  return { deadline, directory, env, marker };
}

test('expiry helper stores a future deadline and leaves the host running', async (context) => {
  const item = await fixture();
  context.after(() => rm(item.directory, { recursive: true, force: true }));
  const deadline = new Date(Date.now() + 60 * 60 * 1000).toISOString();

  await execFileAsync(process.execPath, [helper, 'set', deadline], { env: item.env });
  await execFileAsync(process.execPath, [helper], { env: item.env });

  assert.equal((await readFile(item.deadline, 'utf8')).trim(), deadline);
  await assert.rejects(access(item.marker));
});

test('expiry helper shuts the host down after the deadline', async (context) => {
  const item = await fixture();
  context.after(() => rm(item.directory, { recursive: true, force: true }));
  await writeFile(item.deadline, '2000-01-01T00:00:00.000Z\n');

  await execFileAsync(process.execPath, [helper], { env: item.env });

  assert.equal(await readFile(item.marker, 'utf8'), 'called');
});
