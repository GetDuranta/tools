#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import { chmod, mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { uptime } from 'node:os';
import { dirname } from 'node:path';
import { pathToFileURL } from 'node:url';

const deadlinePath = process.env.DURANTA_PREVIEW_DEADLINE ?? '/var/lib/duranta-preview/deadline';

function parseDeadline(value) {
  const deadline = new Date(String(value).trim());
  if (!Number.isFinite(deadline.getTime())) throw new Error(`Invalid deadline: ${value}`);
  return deadline;
}

function shutdown() {
  const result = spawnSync('shutdown', ['-h', 'now'], { stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`shutdown exited with ${result.status}`);
}

async function setDeadline(value) {
  const deadline = parseDeadline(value);
  if (deadline <= new Date()) throw new Error('Deadline must be in the future');
  await mkdir(dirname(deadlinePath), { recursive: true });
  const temporary = `${deadlinePath}.${process.pid}`;
  await writeFile(temporary, `${deadline.toISOString()}\n`, { mode: 0o600 });
  await chmod(temporary, 0o600);
  await rename(temporary, deadlinePath);
  console.log(deadline.toISOString());
}

async function checkDeadline() {
  let deadline;
  try {
    deadline = parseDeadline(await readFile(deadlinePath, 'utf8'));
  } catch {
    if (uptime() < 3600) return;
    console.error('Missing or invalid preview deadline; shutting down');
    shutdown();
    return;
  }
  if (deadline <= new Date()) shutdown();
}

export async function main(argv = process.argv.slice(2)) {
  if (argv[0] === 'set' && argv.length === 2) {
    await setDeadline(argv[1]);
    return;
  }
  if (argv.length === 0) {
    await checkDeadline();
    return;
  }
  throw new Error('Usage: duranta-preview-expiry [set <ISO deadline>]');
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
