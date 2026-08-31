import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

const script = new URL('./patch-logto.mjs', import.meta.url).pathname;

async function validFixture() {
  const directory = await mkdtemp(join(tmpdir(), 'duranta-logto-patch-'));
  await writeFile(join(directory, 'bootstrap.mjs'), `
const env = {...process.env};
const BRAND_LOGO_DATA_URL = 'https://local.getduranta.com/static/images/logo.svg';
const SPA_REDIRECT_URIS = ['https://local.getduranta.com/a/callback'];
const SPA_POST_LOGOUT_URIS = ['https://local.getduranta.com/a/signin'];
const SPA_CORS_ORIGINS = [
  'https://local.getduranta.com',
];
`);
  await writeFile(join(directory, 'seed.mjs'), `
const sql = 'seed';
const client = { query() {} };
try {
  await client.query(sql);
} finally {}
`);
  return directory;
}

test('Logto patch is idempotent and fails closed when its source anchors drift', async (context) => {
  const valid = await validFixture();
  const drifted = await mkdtemp(join(tmpdir(), 'duranta-logto-patch-'));
  context.after(() => Promise.all([
    rm(valid, { recursive: true, force: true }),
    rm(drifted, { recursive: true, force: true }),
  ]));

  const first = spawnSync(process.execPath, [script, valid], { encoding: 'utf8' });
  assert.equal(first.status, 0, first.stderr);
  const bootstrap = await readFile(join(valid, 'bootstrap.mjs'), 'utf8');
  const seed = await readFile(join(valid, 'seed.mjs'), 'utf8');
  assert.match(bootstrap, /env\.LOGTO_BRAND_LOGO_URL/);
  assert.match(bootstrap, /env\.LOGTO_EXTRA_SPA_ORIGINS/);
  assert.match(seed, /UPDATE application_secrets SET value = \$1/);

  const repeated = spawnSync(process.execPath, [script, valid], { encoding: 'utf8' });
  assert.equal(repeated.status, 0, repeated.stderr);
  assert.equal((await readFile(join(valid, 'seed.mjs'), 'utf8')).match(/const previewM2MSecret/g)?.length, 1);

  await writeFile(join(drifted, 'bootstrap.mjs'), 'const unrelated = true;\n');
  await writeFile(join(drifted, 'seed.mjs'), 'const unrelated = true;\n');
  const drift = spawnSync(process.execPath, [script, drifted], { encoding: 'utf8' });
  assert.notEqual(drift.status, 0);
  assert.match(drift.stderr, /shim no longer applies cleanly/);
});
