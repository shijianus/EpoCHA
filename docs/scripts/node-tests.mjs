#!/usr/bin/env node
import { statSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

export function assertTestFiles(args, cwd = process.cwd()) {
  const files = args.filter((arg) => arg.endsWith('.test.mjs'));
  if (!files.length) throw new Error('explicit test files required');
  for (const file of files) {
    let valid = false;
    try { valid = statSync(resolve(cwd, file)).isFile(); } catch { /* fail closed below */ }
    if (!valid) throw new Error(`missing test file: ${file}`);
  }
  return files;
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const args = process.argv.slice(2);
    assertTestFiles(args);
    const result = spawnSync(process.execPath, ['--test', ...args], { stdio: 'inherit' });
    if (result.error) throw result.error;
    process.exitCode = result.status ?? 1;
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
