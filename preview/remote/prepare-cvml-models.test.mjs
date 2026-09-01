import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { cpuModelsConfig, prepareCvmlModels } from './prepare-cvml-models.mjs';

const MODELS = `segmentation:
  residential:
    use_fp16: false
    use_compile: false
  sliced_commercial:
    use_fp16: true  # CUDA only
    use_compile: true
  another_model:
    use_fp16: true
depth:
  metric:
    use_compile: true
`;

test('changes only sliced commercial CUDA settings and is idempotent', () => {
  const once = cpuModelsConfig(MODELS);
  assert.match(once, /sliced_commercial:\n    use_fp16: false  # CUDA only\n    use_compile: false/);
  assert.match(once, /another_model:\n    use_fp16: true/);
  assert.match(once, /depth:\n  metric:\n    use_compile: true/);
  assert.equal(cpuModelsConfig(once), once);
});

test('fails closed when the expected model schema drifts', () => {
  assert.throws(
    () => cpuModelsConfig(MODELS.replace('    use_compile: true\n  another_model:', '  another_model:')),
    /expected one sliced_commercial block/,
  );
});

test('updates an existing runtime file in place with mode 0600', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'preview-cvml-models-'));
  const source = join(directory, 'models.yaml');
  const destination = join(directory, 'models.cpu.yaml');
  try {
    await writeFile(source, MODELS);
    await writeFile(destination, 'old');
    const before = await stat(destination);
    await prepareCvmlModels(source, destination);
    const after = await stat(destination);
    assert.equal(after.ino, before.ino);
    assert.equal(after.mode & 0o777, 0o600);
    assert.equal(await readFile(destination, 'utf8'), cpuModelsConfig(MODELS));
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
