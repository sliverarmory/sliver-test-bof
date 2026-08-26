#!/usr/bin/env node

'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function warning(message) {
  process.stdout.write(`::warning::${String(message).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A')}\n`);
}

function recordedPids(state) {
  const values = [];
  const add = (candidate) => {
    const pid = Number(typeof candidate === 'object' && candidate !== null ? candidate.pid : candidate);
    if (Number.isSafeInteger(pid) && pid > 1 && pid !== process.pid) values.push(pid);
  };

  for (const key of ['processes', 'pids', 'implant_pids']) {
    if (Array.isArray(state[key])) state[key].forEach(add);
  }
  for (const key of ['server_pid', 'listener_pid', 'implant_pid']) {
    if (state[key] !== undefined) add(state[key]);
  }
  return [...new Set(values)];
}

function signalPid(pid, force) {
  if (process.platform === 'win32') {
    try {
      process.kill(pid, 0);
    } catch (error) {
      if (error.code === 'ESRCH') return;
      throw error;
    }
    const arguments_ = ['/PID', String(pid), '/T'];
    if (force) arguments_.push('/F');
    const result = spawnSync('taskkill.exe', arguments_, {
      encoding: 'utf8',
      windowsHide: true,
    });
    if (result.status !== 0 && !/not found|no running instance/i.test(`${result.stdout}\n${result.stderr}`)) {
      throw new Error(`taskkill exited ${result.status}: ${result.stderr || result.stdout}`);
    }
    return;
  }

  try {
    // The Go harness starts each recorded process as its own process-group
    // leader, so the negative ID terminates only that recorded process tree.
    process.kill(-pid, force ? 'SIGKILL' : 'SIGTERM');
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}

function pause(milliseconds) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, milliseconds);
}

function cleanupCandidates(state) {
  const candidates = [];
  for (const key of ['cleanup_paths', 'cleanupPaths', 'temp_dirs', 'tempDirs']) {
    if (Array.isArray(state[key])) candidates.push(...state[key]);
  }
  return [...new Set(candidates.filter((item) => typeof item === 'string' && item.trim() !== ''))];
}

function isSafeCleanupPath(candidate, stateFile, state) {
  const resolved = path.resolve(candidate);
  const runnerTemp = path.resolve(process.env.RUNNER_TEMP || os.tmpdir());
  const workspace = path.resolve(process.env.GITHUB_WORKSPACE || state.workspace || process.cwd());
  const allowedWorkspaceTemp = path.join(workspace, '.sliver-bof-e2e', 'tmp');
  const inside = (child, parent) => child !== parent && child.startsWith(`${parent}${path.sep}`);

  if (resolved === path.parse(resolved).root || resolved === workspace || resolved === path.dirname(workspace)) return false;
  if (resolved === path.resolve(state.results_dir || path.join(workspace, '.sliver-bof-e2e', 'results'))) return false;
  if (resolved === path.resolve(stateFile)) return false;
  return inside(resolved, runnerTemp) || inside(resolved, allowedWorkspaceTemp);
}

function main() {
  const stateFile = process.env.STATE_state_file;
  const preparedDir = process.env.STATE_prepared_dir;
  if (preparedDir) {
    const runnerTemp = path.resolve(process.env.RUNNER_TEMP || os.tmpdir());
    const resolved = path.resolve(preparedDir);
    if (resolved !== runnerTemp && resolved.startsWith(`${runnerTemp}${path.sep}`)) {
      try {
        fs.rmSync(resolved, { recursive: true, force: true, maxRetries: 2 });
      } catch (error) {
        warning(`Could not remove prepared acquisition directory ${resolved}: ${error.message}`);
      }
    } else {
      warning(`Refusing prepared acquisition directory outside runner temp: ${preparedDir}`);
    }
  }
  if (!stateFile || !fs.existsSync(stateFile)) return;

  let state;
  try {
    state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  } catch (error) {
    warning(`Could not read cleanup state ${stateFile}: ${error.message}`);
    return;
  }

  const pids = recordedPids(state);
  for (const pid of pids) {
    try {
      signalPid(pid, process.platform === 'win32');
    } catch (error) {
      warning(`Could not stop recorded process ${pid}: ${error.message}`);
    }
  }
  if (pids.length > 0 && process.platform !== 'win32') pause(750);
  if (process.platform !== 'win32') {
    for (const pid of pids) {
      try {
        signalPid(pid, true);
      } catch (error) {
        warning(`Could not kill recorded process ${pid}: ${error.message}`);
      }
    }
  }

  for (const candidate of cleanupCandidates(state)) {
    if (!isSafeCleanupPath(candidate, stateFile, state)) {
      warning(`Refusing unsafe cleanup path recorded in state: ${candidate}`);
      continue;
    }
    try {
      fs.rmSync(path.resolve(candidate), { recursive: true, force: true, maxRetries: 2 });
    } catch (error) {
      warning(`Could not remove recorded temporary path ${candidate}: ${error.message}`);
    }
  }

  try {
    fs.rmSync(stateFile, { force: true });
  } catch (error) {
    warning(`Could not remove cleanup state ${stateFile}: ${error.message}`);
  }
}

main();
