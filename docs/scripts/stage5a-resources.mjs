import { spawnSync } from 'node:child_process';

export function registerOwnedResource(state, kind, id) {
  if (!state || !Array.isArray(state[kind]) || typeof id !== 'string' || id.length === 0) {
    throw new Error('resource ownership requires a known collection and nonempty runtime ID');
  }
  if (!state[kind].includes(id)) state[kind].push(id);
}

export function cleanupOwnedResources(state, docker = 'docker', run = spawnSync) {
  if (!state || state.armed !== true) return { status: 0, calls: [] };
  const calls = [];
  for (const id of state.containers || []) calls.push(['rm', '-f', id]);
  for (const id of state.volumes || []) calls.push(['volume', 'rm', id]);
  for (const id of state.networks || []) calls.push(['network', 'rm', id]);
  for (const id of state.pulledImages || []) calls.push(['image', 'rm', id]);
  for (const args of calls) {
    const result = run(docker, args, { encoding: 'utf8', windowsHide: true });
    if (result.error || result.signal || result.status !== 0) {
      return { status: 1, failed: args, exit_code: result.status ?? null, calls };
    }
  }
  return { status: 0, calls };
}
