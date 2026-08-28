#!/usr/bin/env node

import { chmod, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const deadlinePath = process.env.DURANTA_PREVIEW_DEADLINE ?? '/var/lib/duranta-preview/deadline';

export function parseDuration(value) {
  const input = String(value ?? '').trim().toLowerCase();
  const units = { s: 1, m: 60, h: 3600, d: 86400, w: 604800 };
  const pattern = /(\d+)([smhdw])/gy;
  let offset = 0;
  let seconds = 0;
  let match;
  while ((match = pattern.exec(input))) {
    if (match.index !== offset) throw new Error(`Invalid duration: ${value}`);
    seconds += Number(match[1]) * units[match[2]];
    offset = pattern.lastIndex;
  }
  if (!input || offset !== input.length || seconds <= 0 || !Number.isSafeInteger(seconds)) {
    throw new Error(`Invalid duration: ${value}`);
  }
  return seconds;
}

export function extendDeadline(current, duration, now = new Date()) {
  const currentDate = new Date(current);
  if (!Number.isFinite(currentDate.getTime())) throw new Error(`Invalid deadline: ${current}`);
  const base = Math.max(currentDate.getTime(), now.getTime());
  return new Date(base + parseDuration(duration) * 1000).toISOString();
}

async function readDeadline() {
  return (await readFile(deadlinePath, 'utf8')).trim();
}

async function writeDeadline(value) {
  const temporary = join(dirname(deadlinePath), `.deadline-${process.pid}`);
  await writeFile(temporary, `${value}\n`, { mode: 0o600 });
  await chmod(temporary, 0o600);
  await rename(temporary, deadlinePath);
}

export async function main(argv = process.argv.slice(2)) {
  const [command, value] = argv;
  if (command === 'show' && !value) {
    console.log(await readDeadline());
    return;
  }
  if (command !== 'extend' || !value || argv.length !== 2) {
    throw new Error('Usage: duranta-preview-ttl show | duranta-preview-ttl extend <duration>');
  }
  const deadline = extendDeadline(await readDeadline(), value);
  await writeDeadline(deadline);
  console.log(deadline);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
