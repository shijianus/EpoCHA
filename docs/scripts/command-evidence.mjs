import { spawnSync } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';

// One receipt per direct subprocess, including launch failures. No environment dump.
export function createCommandRecorder(directory) {
  const commands = [];
  const hash = (bytes) => createHash('sha256').update(bytes).digest('hex');
  return {
    commands,
    run(command, args, options = {}, check = null) {
      const id = `command-${String(commands.length + 1).padStart(4, '0')}`;
      const started = new Date();
      const result = spawnSync(command, args, options);
      const finished = new Date();
      const receipt = { id, check, command: [command, ...args], started_at: started.toISOString(), finished_at: finished.toISOString(), duration_ms: finished - started,
        raw_exit_code: result.status, signal: result.signal, error: result.error?.message ?? null };
      for (const stream of ['stdout', 'stderr']) {
        const bytes = result[stream] ?? '';
        receipt[stream] = `${id}.${stream}.log`;
        receipt[`${stream}_sha256`] = hash(bytes);
        writeFileSync(join(directory, receipt[stream]), bytes, { flag: 'wx', mode: 0o600 });
      }
      writeFileSync(join(directory, `${id}.json`), `${JSON.stringify(receipt, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
      commands.push(receipt);
      return result;
    },
  };
}
