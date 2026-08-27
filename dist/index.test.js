'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const test = require('node:test');

const {
  childEnvironment,
  clearCredentialInputs,
} = require('./index.js');

test('childEnvironment passes only the controller allowlist', () => {
  const source = {
    PATH: '/tools',
    HOME: '/private/home',
    RUNNER_TEMP: '/private/temp',
    HTTPS_PROXY: 'https://proxy.test',
    GITHUB_TOKEN: 'secret-token',
    'INPUT_GITHUB-TOKEN': 'secret-input',
    AWS_SECRET_ACCESS_KEY: 'secret-cloud',
    GIT_CONFIG_COUNT: '1',
  };
  const acquisition = childEnvironment(source, true);
  assert.equal(acquisition.PATH, '/tools');
  assert.equal(acquisition.HTTPS_PROXY, 'https://proxy.test');
  assert.equal(acquisition.GITHUB_TOKEN, undefined);
  assert.equal(acquisition['INPUT_GITHUB-TOKEN'], undefined);
  assert.equal(acquisition.AWS_SECRET_ACCESS_KEY, undefined);
  assert.equal(acquisition.GIT_CONFIG_COUNT, undefined);
  assert.equal(acquisition.GOENV, 'off');
  assert.equal(acquisition.GOWORK, 'off');
  assert.equal(acquisition.GOFLAGS, '');

  const execution = childEnvironment(source, false);
  assert.equal(execution.HTTPS_PROXY, undefined);
  assert.equal(execution.GOPROXY, 'off');
  assert.equal(execution.GOSUMDB, 'off');
  assert.equal(execution.GOTOOLCHAIN, 'local');
});

test('clearCredentialInputs overwrites and removes token inputs', () => {
  const environment = {
    GITHUB_TOKEN: 'token',
    'INPUT_GITHUB-TOKEN': 'input-token',
    INPUT_GITHUB_TOKEN: 'compat-token',
    PATH: '/tools',
  };
  clearCredentialInputs(environment);
  assert.deepEqual(environment, { PATH: '/tools' });
});

test('prepare-caller runs the caller command with plan metadata and no command-file credentials', {
  skip: process.platform === 'win32' ? 'planner hook runs on Ubuntu' : false,
}, () => {
  const result = spawnSync('bash', [path.join(__dirname, 'prepare-caller.sh')], {
    cwd: process.cwd(),
    encoding: 'utf8',
    env: {
      ...process.env,
      ACTIONS_RUNTIME_TOKEN: 'service-secret',
      GITHUB_ENV: '/tmp/github-env-command',
      GITHUB_OUTPUT: '/tmp/github-output-command',
      GITHUB_PATH: '/tmp/github-path-command',
      GITHUB_STATE: '/tmp/github-state-command',
      GITHUB_TOKEN: 'github-secret',
      SLIVER_BOF_E2E_ARCH: '',
      SLIVER_BOF_E2E_OS: '',
      SLIVER_BOF_E2E_PHASE: 'plan',
      SLIVER_BOF_E2E_PREPARE_COMMAND: 'env | sort',
      SLIVER_BOF_E2E_TARGET: '',
    },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^SLIVER_BOF_E2E_PHASE=plan$/m);
  for (const name of [
    'ACTIONS_RUNTIME_TOKEN', 'GITHUB_ENV', 'GITHUB_OUTPUT', 'GITHUB_PATH',
    'GITHUB_STATE', 'GITHUB_TOKEN', 'SLIVER_BOF_E2E_PREPARE_COMMAND',
  ]) {
    assert.doesNotMatch(result.stdout, new RegExp(`^${name}=`, 'm'));
  }
});

test('manifest schema rejects non-portable fixture paths and constrains TCP listeners to typed args', () => {
  const schema = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'schema', 'sliver-bof-e2e-v1.schema.json'), 'utf8'));
  const pattern = new RegExp(schema.$defs.fixtures.properties.directory.properties.files.propertyNames.pattern);
  for (const value of ['marker.txt', '.hidden/child.txt', 'unicode/café.txt']) {
    assert.equal(pattern.test(value), true, value);
  }
  for (const value of ['../escape', 'nested\\escape', 'bad?.txt', 'CON.txt', 'dir./child.txt']) {
    assert.equal(pattern.test(value), false, value);
  }
  const objectPattern = new RegExp(schema.$defs.test.properties.object.pattern);
  for (const value of ['build/windows/amd64/test.o', '.objects/test.o']) {
    assert.equal(objectPattern.test(value), true, value);
  }
  for (const value of ['/tmp/test.o', 'C:/temp/test.o', '../test.o', 'build\\test.o']) {
    assert.equal(objectPattern.test(value), false, value);
  }
  for (const name of ['test', 'collection']) {
    const conditions = schema.$defs[name].allOf;
    assert.ok(conditions.some((condition) => (
      condition.if?.required?.includes('tcp_listener')
      && condition.then?.required?.includes('args')
      && condition.then?.not?.required?.includes('args_raw_base64')
    )), `${name} does not constrain tcp_listener arguments`);
  }
  assert.ok(schema.$defs.override.allOf.some((condition) => (
    condition.if?.required?.includes('tcp_listener')
    && !condition.then?.required?.includes('args')
    && condition.then?.not?.required?.includes('args_raw_base64')
  )), 'override does not allow inherited typed args for tcp_listener');
  assert.ok(schema.$defs.collection.allOf.some((condition) => (
    condition.if?.required?.includes('tcp_listener')
    && condition.then?.properties?.overrides?.additionalProperties
  )), 'collection does not constrain raw-argument overrides of an inherited tcp_listener');
});

test('prepared runtime replaces its environment before executing BOFs', {
  skip: process.platform === 'win32' ? 'POSIX shell fixture' : false,
}, () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'sliver-bof-runtime-test-'));
  try {
    const controller = path.join(directory, 'controller');
    const descriptor = path.join(directory, 'prepared.json');
    const state = path.join(directory, 'state.json');
    fs.writeFileSync(controller, `#!/bin/sh
env | sort > "${controller}.env"
printf '%s\\n' "$@" > "${controller}.args"
`, { mode: 0o700 });
    fs.writeFileSync(descriptor, '{}\n', { mode: 0o600 });

    const result = spawnSync('bash', [path.join(__dirname, 'run-prepared.sh')], {
      cwd: directory,
      encoding: 'utf8',
      env: {
        ...process.env,
        ACTIONS_RUNTIME_TOKEN: 'automatic-service-secret',
        AWS_SECRET_ACCESS_KEY: 'unrelated-job-secret',
        GITHUB_TOKEN: 'forwarded-token',
        GITHUB_OUTPUT: path.join(directory, 'workflow-output'),
        'INPUT_GITHUB-TOKEN': 'action-input-secret',
        RUNNER_TEMP: directory,
        GITHUB_WORKSPACE: directory,
        SLIVER_BOF_ACTION_VERSION: 'owner/action@commit',
        SLIVER_BOF_CONTROLLER: controller,
        SLIVER_BOF_MANIFEST: 'manifest.yml',
        SLIVER_BOF_PREPARED_FILE: descriptor,
        SLIVER_BOF_RESULTS_DIR: path.join(directory, 'results'),
        SLIVER_BOF_SLIVER_VERSION: 'master',
        SLIVER_BOF_STATE_FILE: state,
        SLIVER_BOF_TARGET: 'darwin/arm64',
      },
    });
    assert.equal(result.status, 0, result.stderr);

    const runtimeEnvironment = fs.readFileSync(`${controller}.env`, 'utf8');
    for (const name of [
      'ACTIONS_RUNTIME_TOKEN', 'AWS_SECRET_ACCESS_KEY', 'GITHUB_OUTPUT',
      'GITHUB_TOKEN', 'INPUT_GITHUB-TOKEN',
    ]) {
      assert.doesNotMatch(runtimeEnvironment, new RegExp(`^${name}=`, 'm'));
    }
    assert.match(runtimeEnvironment, new RegExp(`^RUNNER_TEMP=${directory}$`, 'm'));

    const arguments_ = fs.readFileSync(`${controller}.args`, 'utf8');
    assert.match(arguments_, /--prepared\n/);
    assert.match(arguments_, /darwin\/arm64\n/);
    assert.doesNotMatch(arguments_, /--github-output/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
