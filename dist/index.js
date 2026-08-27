#!/usr/bin/env node

'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');

const BASE_ENVIRONMENT = new Set([
  'APPDATA', 'COMSPEC', 'GOCACHE', 'GOMODCACHE', 'GOPATH', 'HOME',
  'LANG', 'LC_ALL', 'LC_CTYPE', 'LOCALAPPDATA', 'PATH', 'PATHEXT',
  'RUNNER_TEMP', 'SSL_CERT_DIR', 'SSL_CERT_FILE', 'SYSTEMDRIVE',
  'SYSTEMROOT', 'TEMP', 'TMP', 'TMPDIR', 'TZ', 'USERPROFILE', 'WINDIR',
  'GITHUB_STEP_SUMMARY',
]);
const NETWORK_ENVIRONMENT = new Set([
  'ALL_PROXY', 'HTTPS_PROXY', 'HTTP_PROXY', 'NO_PROXY',
  'GOPROXY', 'GONOPROXY', 'GONOSUMDB', 'GOSUMDB',
]);

function input(name, fallback = '') {
  const key = `INPUT_${name.replace(/ /g, '_').toUpperCase()}`;
  // GitHub preserves hyphens in JavaScript-action input environment keys. The
  // underscore form keeps local invocation and older runner shims convenient.
  const value = process.env[key] === undefined
    ? process.env[key.replace(/-/g, '_')]
    : process.env[key];
  return value === undefined || value.trim() === '' ? fallback : value.trim();
}

function appendFileCommand(file, name, value) {
  if (!file) return;
  const delimiter = `sliver_bof_e2e_${crypto.randomUUID()}`;
  fs.appendFileSync(file, `${name}<<${delimiter}${os.EOL}${value}${os.EOL}${delimiter}${os.EOL}`, 'utf8');
}

function notice(message) {
  process.stdout.write(`::notice::${String(message).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A')}\n`);
}

function fail(message) {
  process.stderr.write(`::error::${String(message).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A')}\n`);
}

function childEnvironment(source, network) {
  const allowed = new Set(BASE_ENVIRONMENT);
  if (network) {
    for (const name of NETWORK_ENVIRONMENT) allowed.add(name);
  }

  const exact = new Map();
  const variants = new Map();
  const conflictingVariants = new Set();
  for (const [name, value] of Object.entries(source)) {
    const canonical = name.toUpperCase();
    if (!allowed.has(canonical) || value === undefined) continue;
    if (name === canonical) {
      exact.set(canonical, value);
      continue;
    }
    if (variants.has(canonical) && variants.get(canonical) !== value) {
      conflictingVariants.add(canonical);
    } else if (!variants.has(canonical)) {
      variants.set(canonical, value);
    }
  }

  const result = {};
  for (const name of allowed) {
    if (exact.has(name)) {
      result[name] = exact.get(name);
    } else if (conflictingVariants.has(name)) {
      throw new Error(`conflicting case variants for environment variable ${name}`);
    } else if (variants.has(name)) {
      result[name] = variants.get(name);
    }
  }
  if (result.PATH === undefined) {
    throw new Error('PATH is required to launch the Go controller');
  }
  // Do not consult caller-controlled persistent Go configuration or ambient
  // workspaces when compiling the trusted action controller.
  result.GOENV = 'off';
  result.GOWORK = 'off';
  result.GOFLAGS = '';
  if (!network) {
    // prepare compiled this exact controller already, so execution must be
    // satisfiable from the local module/tool caches without network access.
    result.GOPROXY = 'off';
    result.GOSUMDB = 'off';
    result.GOTOOLCHAIN = 'local';
  }
  return result;
}

function clearCredentialInputs(environment = process.env) {
  const sensitive = new Set([
    'GITHUB_TOKEN', 'INPUT_GITHUB-TOKEN', 'INPUT_GITHUB_TOKEN',
  ]);
  for (const name of Object.keys(environment)) {
    if (sensitive.has(name.toUpperCase())) {
      environment[name] = '';
      delete environment[name];
    }
  }
}

function selectedActionVersion() {
  const value = input('action-version')
    || String(process.env.SLIVER_BOF_E2E_ACTION_VERSION || '').trim()
    || String(process.env.SLIVER_BOF_ACTION_VERSION || '').trim()
    || (process.env.GITHUB_ACTION_REPOSITORY && process.env.GITHUB_ACTION_REF
      ? `${process.env.GITHUB_ACTION_REPOSITORY}@${process.env.GITHUB_ACTION_REF}`
      : String(process.env.GITHUB_ACTION_REPOSITORY || '').trim())
    || 'development';
  if (value.length > 512 || /[\0\r\n]/.test(value)) {
    throw new Error('action-version must be a single line of at most 512 bytes');
  }
  return value;
}

function findActionRoot() {
  const candidates = [
    process.env.GITHUB_ACTION_PATH,
    path.resolve(__dirname, '..'),
  ].filter(Boolean);

  for (const candidate of candidates) {
    let current = path.resolve(candidate);
    while (true) {
      if (fs.existsSync(path.join(current, 'go.mod')) && fs.existsSync(path.join(current, 'cmd', 'sliver-bof-e2e'))) {
        return current;
      }
      const parent = path.dirname(current);
      if (parent === current) break;
      current = parent;
    }
  }
  throw new Error('could not locate the action source root containing go.mod and cmd/sliver-bof-e2e');
}

function absoluteFromWorkspace(value, workspace) {
  return path.isAbsolute(value) ? path.normalize(value) : path.resolve(workspace, value);
}

function initializeState(stateFile, workspace, resultsDir) {
  fs.mkdirSync(path.dirname(stateFile), { recursive: true });
  if (fs.existsSync(stateFile)) return;
  const state = {
    version: 1,
    workspace,
    results_dir: resultsDir,
    processes: [],
    cleanup_paths: [],
  };
  fs.writeFileSync(stateFile, `${JSON.stringify(state, null, 2)}\n`, { encoding: 'utf8', mode: 0o600 });
}

async function launchExecutable(executable, args, cwd, env, onSpawn) {
  const child = spawn(executable, args, {
    cwd,
    env,
    stdio: 'inherit',
    windowsHide: true,
  });
  if (onSpawn) onSpawn();

  const forward = (signal) => {
    if (!child.killed) child.kill(signal);
  };
  const interrupt = () => forward('SIGINT');
  const terminate = () => forward('SIGTERM');
  process.once('SIGINT', interrupt);
  process.once('SIGTERM', terminate);
  try {
    return await new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', (code, signal) => resolve(code === null ? (signal ? 1 : 0) : code));
    });
  } finally {
    process.removeListener('SIGINT', interrupt);
    process.removeListener('SIGTERM', terminate);
  }
}

async function launchGo(actionRoot, args, env, onSpawn) {
  return launchExecutable('go', args, actionRoot, env, onSpawn);
}

async function run() {
  const mode = input('mode', 'prepare').toLowerCase();
  if (!['plan', 'prepare', 'aggregate'].includes(mode)) {
    throw new Error(`unsupported mode ${JSON.stringify(mode)}; expected plan, prepare, or aggregate`);
  }

  const actionRoot = findActionRoot();
  const workspace = path.resolve(process.env.GITHUB_WORKSPACE || process.cwd());
  const manifest = absoluteFromWorkspace(input('manifest', '.github/sliver-bof-e2e.yml'), workspace);
  const objectRootInput = input('object-root');
  const objectRoot = objectRootInput ? absoluteFromWorkspace(objectRootInput, workspace) : '';
  const resultsDir = absoluteFromWorkspace(input('results-dir', '.sliver-bof-e2e/results'), workspace);
  const target = input('target');
  const sliverVersion = input('sliver-version');
  const serverInput = input('sliver-server-path');
  const serverPath = serverInput ? absoluteFromWorkspace(serverInput, workspace) : '';
  const actionVersion = selectedActionVersion();
  const githubOutput = process.env.GITHUB_OUTPUT || '';
  const runnerTemp = path.resolve(process.env.RUNNER_TEMP || os.tmpdir());
  const stateFile = path.join(runnerTemp, `sliver-bof-e2e-${crypto.randomUUID()}.json`);

  fs.mkdirSync(resultsDir, { recursive: true });
  fs.mkdirSync(runnerTemp, { recursive: true });
  const controllerArgs = (command) => [
    command,
    '--manifest',
    manifest,
    '--workspace',
    workspace,
    '--results-dir',
    resultsDir,
    '--action-version',
    actionVersion,
    ...(objectRoot ? ['--object-root', objectRoot] : []),
  ];
  const goRunArgs = (command) => [
    'run',
    '-mod=readonly',
    '-tags',
    'client,go_sqlite',
    './cmd/sliver-bof-e2e',
    ...controllerArgs(command),
  ];

  appendFileCommand(githubOutput, 'results-dir', resultsDir);
  notice(`Sliver BOF E2E ${mode}: ${target || 'manifest matrix'}`);

  if (mode !== 'prepare') {
    const args = goRunArgs(mode);
    let objectsDir = '';
    if (mode === 'plan') {
      objectsDir = fs.mkdtempSync(path.join(runnerTemp, 'sliver-bof-e2e-objects-'));
      fs.chmodSync(objectsDir, 0o700);
      args.push('--objects-dir', objectsDir);
    }
    if (githubOutput) args.push('--github-output', githubOutput);
    if (mode === 'plan' && sliverVersion) args.push('--sliver-version', sliverVersion);
    // Both commands are compiled on fresh jobs. Allow dependency acquisition
    // for that trusted build; only plan receives the optional GitHub token.
    const env = childEnvironment(process.env, true);
    let token = mode === 'plan' ? input('github-token') : '';
    if (token) env.GITHUB_TOKEN = token;
    try {
      const exitCode = await launchGo(actionRoot, args, env, () => {
        clearCredentialInputs();
        delete env.GITHUB_TOKEN;
        token = '';
      });
      if (exitCode !== 0) {
        process.exitCode = exitCode;
        if (objectsDir) fs.rmSync(objectsDir, { recursive: true, force: true, maxRetries: 2 });
      }
    } catch (error) {
      if (objectsDir) fs.rmSync(objectsDir, { recursive: true, force: true, maxRetries: 2 });
      throw error;
    }
    return;
  }

  const preparedDir = fs.mkdtempSync(path.join(runnerTemp, 'sliver-bof-e2e-prepared-'));
  const preparedFile = path.join(preparedDir, 'prepared.json');
  const controller = path.join(preparedDir, process.platform === 'win32' ? 'sliver-bof-e2e.exe' : 'sliver-bof-e2e');
  let prepared = false;
  try {
    fs.chmodSync(preparedDir, 0o700);
    initializeState(stateFile, workspace, resultsDir);
    appendFileCommand(githubOutput, 'action-root', actionRoot);
    appendFileCommand(githubOutput, 'action-version', actionVersion);
    appendFileCommand(githubOutput, 'controller', controller);
    appendFileCommand(githubOutput, 'node', process.execPath);
    appendFileCommand(githubOutput, 'prepared-dir', preparedDir);
    appendFileCommand(githubOutput, 'prepared-file', preparedFile);
    appendFileCommand(githubOutput, 'state-file', stateFile);

    const prepareEnv = childEnvironment(process.env, true);
    const buildExit = await launchGo(actionRoot, [
      'build',
      '-mod=readonly',
      '-tags',
      'client,go_sqlite',
      '-buildvcs=false',
      '-trimpath',
      '-o',
      controller,
      './cmd/sliver-bof-e2e',
    ], prepareEnv);
    if (buildExit !== 0) {
      process.exitCode = buildExit;
      return;
    }

    const prepareArgs = controllerArgs('prepare');
    if (githubOutput) prepareArgs.push('--github-output', githubOutput);
    if (sliverVersion) prepareArgs.push('--sliver-version', sliverVersion);
    if (target) prepareArgs.push('--target', target);
    if (serverPath) prepareArgs.push('--server-path', serverPath);
    prepareArgs.push('--prepared', preparedFile);

    let token = input('github-token');
    if (token) prepareEnv.GITHUB_TOKEN = token;
    const prepareExit = await launchExecutable(controller, prepareArgs, actionRoot, prepareEnv, () => {
      clearCredentialInputs();
      delete prepareEnv.GITHUB_TOKEN;
      token = '';
    });
    if (prepareExit !== 0) {
      process.exitCode = prepareExit;
      return;
    }
    prepared = true;
  } finally {
    clearCredentialInputs();
    if (!prepared) {
      fs.rmSync(preparedDir, { recursive: true, force: true, maxRetries: 2 });
      fs.rmSync(stateFile, { force: true });
    }
  }
}

if (require.main === module) {
  run().catch((error) => {
    fail(error && error.stack ? error.stack : error);
    process.exitCode = 1;
  });
}

module.exports = {
  childEnvironment,
  clearCredentialInputs,
  selectedActionVersion,
};
