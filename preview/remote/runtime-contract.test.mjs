import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (name) => readFile(new URL(name, import.meta.url), 'utf8');

test('public runtime keeps containers rootless and waits for local Logto', async () => {
  const compose = await read('./compose.preview.yml');
  assert.doesNotMatch(compose, /privileged:|SYS_ADMIN|--otel=false/);
  assert.match(compose, /react-router dev \. --mode live/);
  assert.match(compose, /logto:\n\s+condition: service_healthy/);
  assert.match(compose, /LOGTO_M2M_APP_SECRET: \$\{LOGTO_M2M_APP_SECRET\}/);
  assert.match(compose, /RUSTFS_ACCESS_KEY: \$\{RUSTFS_ACCESS_KEY\}/);
});

test('bootstrap fails closed and replaces inherited shared credentials', async () => {
  const bootstrap = await read('./bootstrap.sh');
  assert.match(bootstrap, /safety_deadline_epoch/);
  assert.match(bootstrap, /getent ahostsv4/);
  assert.match(bootstrap, /InternalApiKey: \$internal_api_key/);
  assert.match(bootstrap, /SessionCookieKey: \$session_cookie_key/);
  assert.match(bootstrap, /OpenRouterApiKey: ''/);
  assert.match(bootstrap, /Sendgrid:\n\s+Enabled: false/);
  assert.match(bootstrap, /VITE_PUBLIC_API_URL=https:\/\/\$hostname/);
  assert.match(bootstrap, /VITE_UPTRACE_LOGS_ENDPOINT=/);
});
