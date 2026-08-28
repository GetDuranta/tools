import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const script = new URL('./patch-logto.mjs', import.meta.url).pathname;

async function fixture() {
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

test('patch adds preview origins, branding, and per-instance M2M rotation', async () => {
  const directory = await fixture();
  try {
    const result = spawnSync(process.execPath, [script, directory], { encoding: 'utf8' });
    assert.equal(result.status, 0, result.stderr);
    const bootstrap = await readFile(join(directory, 'bootstrap.mjs'), 'utf8');
    const seed = await readFile(join(directory, 'seed.mjs'), 'utf8');
    assert.match(bootstrap, /env\.LOGTO_BRAND_LOGO_URL/);
    assert.match(bootstrap, /env\.LOGTO_EXTRA_SPA_ORIGINS/);
    assert.match(bootstrap, /SPA_REDIRECT_URIS\.push/);
    assert.match(bootstrap, /SPA_POST_LOGOUT_URIS\.push/);
    assert.match(bootstrap, /SPA_CORS_ORIGINS\.push/);
    assert.match(seed, /UPDATE application_secrets SET value = \$1/);
    assert.match(seed, /secretUpdate\.rowCount !== 1/);

    const repeated = spawnSync(process.execPath, [script, directory], { encoding: 'utf8' });
    assert.equal(repeated.status, 0, repeated.stderr);
    assert.equal((await readFile(join(directory, 'seed.mjs'), 'utf8')).match(/const previewM2MSecret/g)?.length, 1);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test('patch fails when the expected Logto structures drift', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'duranta-logto-patch-'));
  try {
    await writeFile(join(directory, 'bootstrap.mjs'), 'const unrelated = true;\n');
    await writeFile(join(directory, 'seed.mjs'), 'const unrelated = true;\n');
    const result = spawnSync(process.execPath, [script, directory], { encoding: 'utf8' });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /shim no longer applies cleanly/);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
