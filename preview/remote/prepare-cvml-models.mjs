#!/usr/bin/env node
import { chmod, open, readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

export function cpuModelsConfig(source) {
  const lines = source.split('\n');
  let blockCount = 0;
  let inSlicedCommercial = false;
  let fp16Count = 0;
  let compileCount = 0;

  const updated = lines.map((line) => {
    if (/^  sliced_commercial:\s*(?:#.*)?$/.test(line)) {
      blockCount += 1;
      inSlicedCommercial = true;
      return line;
    }

    const content = line.trimStart();
    if (inSlicedCommercial && content && !content.startsWith('#')) {
      const indentation = line.length - content.length;
      if (indentation <= 2) inSlicedCommercial = false;
    }
    if (!inSlicedCommercial) return line;

    if (/^    use_fp16:\s*/.test(line)) {
      fp16Count += 1;
      return line.replace(/^(\s*use_fp16:\s*)\S+/, '$1false');
    }
    if (/^    use_compile:\s*/.test(line)) {
      compileCount += 1;
      return line.replace(/^(\s*use_compile:\s*)\S+/, '$1false');
    }
    return line;
  });

  if (blockCount !== 1 || fp16Count !== 1 || compileCount !== 1) {
    throw new Error(
      `expected one sliced_commercial block with use_fp16/use_compile, found ${blockCount}/${fp16Count}/${compileCount}`,
    );
  }
  return updated.join('\n');
}

async function writeInPlace(path, content) {
  const buffer = Buffer.from(content);
  let handle;
  try {
    handle = await open(path, 'r+');
    await handle.write(buffer, 0, buffer.length, 0);
    await handle.truncate(buffer.length);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    await writeFile(path, buffer, { mode: 0o600 });
  } finally {
    await handle?.close();
  }
  await chmod(path, 0o600);
}

export async function prepareCvmlModels(sourcePath, destinationPath) {
  const source = await readFile(sourcePath, 'utf8');
  await writeInPlace(destinationPath, cpuModelsConfig(source));
}

async function main(argv) {
  if (argv.length !== 2) {
    throw new Error('Usage: prepare-cvml-models.mjs <source> <destination>');
  }
  await prepareCvmlModels(argv[0], argv[1]);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
