#!/usr/bin/env node

import { readFile, writeFile } from 'node:fs/promises';
import { basename, dirname, join } from 'node:path';

const input = process.argv[2];
if (!input) throw new Error('Logto sandbox path is required');

const sandbox = basename(input) === 'bootstrap.mjs' ? dirname(input) : input;
const bootstrapFile = join(sandbox, 'bootstrap.mjs');
const seedFile = join(sandbox, 'seed.mjs');
let bootstrap = await readFile(bootstrapFile, 'utf8');
let seed = await readFile(seedFile, 'utf8');

const brandBefore = "const BRAND_LOGO_DATA_URL = 'https://local.getduranta.com/static/images/logo.svg';";
const brandAfter = "const BRAND_LOGO_DATA_URL = env.LOGTO_BRAND_LOGO_URL || 'https://local.getduranta.com/static/images/logo.svg';";
if (bootstrap.includes(brandBefore)) {
  bootstrap = bootstrap.replace(brandBefore, brandAfter);
} else if (!bootstrap.includes(brandAfter)) {
  throw new Error('Logto brand shim no longer applies cleanly');
}

if (!bootstrap.includes('env.LOGTO_EXTRA_SPA_ORIGINS')) {
  const corsBlock = /const SPA_CORS_ORIGINS = \[[\s\S]*?\n\];/;
  const match = bootstrap.match(corsBlock);
  if (!match) throw new Error('Logto SPA origin shim no longer applies cleanly');
  const originShim = `${match[0]}

for (const origin of (env.LOGTO_EXTRA_SPA_ORIGINS || '').split(',').map((value) => value.trim()).filter(Boolean)) {
  const url = new URL(origin);
  if (url.protocol !== 'https:' || url.origin !== origin) {
    throw new Error(\`Invalid LOGTO_EXTRA_SPA_ORIGINS value: \${origin}\`);
  }
  SPA_REDIRECT_URIS.push(\`\${origin}/a/callback\`);
  SPA_POST_LOGOUT_URIS.push(\`\${origin}/a/signin\`);
  SPA_CORS_ORIGINS.push(origin);
}`;
  bootstrap = bootstrap.replace(corsBlock, originShim);
}

if (!seed.includes('const previewM2MSecret = process.env.LOGTO_M2M_APP_SECRET;')) {
  const seedAnchor = '  await client.query(sql);';
  if (!seed.includes(seedAnchor)) {
    throw new Error('Logto M2M secret shim no longer applies cleanly');
  }
  seed = seed.replace(seedAnchor, `${seedAnchor}
  const previewM2MSecret = process.env.LOGTO_M2M_APP_SECRET;
  if (!previewM2MSecret) throw new Error('LOGTO_M2M_APP_SECRET is required');
  const secretUpdate = await client.query(
    'UPDATE application_secrets SET value = $1 WHERE tenant_id = $2 AND application_id = $3 AND name = $4',
    [previewM2MSecret, 'default', 'duranta-m2m-local', 'Default secret'],
  );
  if (secretUpdate.rowCount !== 1) {
    throw new Error(\`Expected one Logto M2M secret, updated \${secretUpdate.rowCount}\`);
  }`);
}

await Promise.all([
  writeFile(bootstrapFile, bootstrap),
  writeFile(seedFile, seed),
]);
