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
