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
  assert.doesNotMatch(compose, /RUSTFS_SERVER_DOMAINS/);
  assert.match(compose, /frontend:[\s\S]*?- bash\n\s+- -c\n/);
  assert.match(compose, /PREVIEW_FRONTEND_MODE: \$\{PREVIEW_FRONTEND_MODE:-production\}/);
  assert.match(compose, /production\)[\s\S]*?react-router build[\s\S]*?vite preview/);
  assert.match(compose, /find build\/client -type f -name '\*\.map' -delete/);
  assert.match(compose, /dev\)[\s\S]*?react-router dev/);
  const caddy = await read('./Caddyfile');
  assert.match(caddy, /^\{\$PREVIEW_HOSTNAME\},\nlogto\.\{\$PREVIEW_HOSTNAME\},\ns3\.\{\$PREVIEW_HOSTNAME\},\nmailpit\.\{\$PREVIEW_HOSTNAME\},\nuptrace\.\{\$PREVIEW_HOSTNAME\} \{/);
  assert.match(caddy, /tls \{\$PREVIEW_CERTIFICATE\} \{\$PREVIEW_PRIVATE_KEY\}/);
  assert.equal(caddy.match(/^\s*tls /gm)?.length, 1);
  for (const service of ['app', 'logto', 's3', 'mailpit', 'uptrace']) {
    assert.match(caddy, new RegExp(`handle @${service} \\{`));
  }
  const vite = await read('./vite.preview.mjs');
  assert.match(vite, /build: \{\n\s+sourcemap: false/);
});

test('bootstrap obtains one SAN certificate for every public hostname', async () => {
  const bootstrap = await read('./bootstrap.sh');
  assert.match(bootstrap, /certbot certonly/);
  assert.match(bootstrap, /--cert-name "\$certificate_name"/);
  assert.match(bootstrap, /PREVIEW_CERTIFICATE=\$certificate_live_dir\/fullchain\.pem/);
  assert.match(bootstrap, /PREVIEW_PRIVATE_KEY=\$certificate_live_dir\/privkey\.pem/);
  assert.deepEqual(
    [...bootstrap.matchAll(/--domain "([^"\n]+)"/g)].map((match) => match[1]),
    ['$hostname', 'logto.$hostname', 's3.$hostname', 'mailpit.$hostname', 'uptrace.$hostname'],
  );
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
  assert.match(bootstrap, /s3_endpoint=http:\/\/blobs:9000/);
  assert.match(bootstrap, /s3_endpoint=https:\/\/s3\.\$hostname/);
});

test('bootstrap defines every local RustFS bucket used during backend startup', async () => {
  const bootstrap = await read('./bootstrap.sh');
  assert.match(bootstrap, /BlobsBucket: duranta-blobs-preview/);
  assert.match(bootstrap, /TileCacheBucket: duranta-tile-cache-preview/);
  assert.match(bootstrap, /Repos:\n\s+Cvml:\n\s+BlobBucket: duranta-cvml-preview/);
});

test('Postgres warm image sources its init script and passes an entrypoint smoke test', async () => {
  const build = await read('./build-images.sh');
  assert.match(build, /chmod 0644 "\$postgis_context\/initdb-postgis\.sh"/);
  assert.match(build, /stat -c %a \/docker-entrypoint-initdb\.d\/10_postgis\.sh/);
  assert.match(build, /postgis:golden postgres --version/);
});
