#!/usr/bin/env bash

set -euo pipefail

: "${SLIVER_BOF_E2E_PREPARE_COMMAND:?caller preparation command is required}"

# Caller preparation is trusted code, but it does not need acquisition
# credentials or GitHub's persistent command files. Run it with an explicit
# build-oriented environment so accidental command-file writes or inherited
# secrets cannot affect the later token-bearing acquisition step.
exec env -i \
	PATH="${PATH}" \
	HOME="${HOME:-}" \
	RUNNER_TEMP="${RUNNER_TEMP:-}" \
	GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-${PWD}}" \
	RUNNER_OS="${RUNNER_OS:-Linux}" \
	RUNNER_ARCH="${RUNNER_ARCH:-X64}" \
	CI="${CI:-true}" \
	GOROOT="${GOROOT:-}" \
	GOPATH="${GOPATH:-}" \
	GOCACHE="${GOCACHE:-}" \
	GOMODCACHE="${GOMODCACHE:-}" \
	CC="${CC:-}" \
	CXX="${CXX:-}" \
	AR="${AR:-}" \
	TMP="${TMP:-}" \
	TEMP="${TEMP:-}" \
	TMPDIR="${TMPDIR:-}" \
	LANG="${LANG:-}" \
	LC_ALL="${LC_ALL:-}" \
	LC_CTYPE="${LC_CTYPE:-}" \
	TZ="${TZ:-}" \
	HTTP_PROXY="${HTTP_PROXY:-}" \
	HTTPS_PROXY="${HTTPS_PROXY:-}" \
	ALL_PROXY="${ALL_PROXY:-}" \
	NO_PROXY="${NO_PROXY:-}" \
	SLIVER_BOF_E2E_PHASE="${SLIVER_BOF_E2E_PHASE}" \
	SLIVER_BOF_E2E_TARGET="${SLIVER_BOF_E2E_TARGET}" \
	SLIVER_BOF_E2E_OS="${SLIVER_BOF_E2E_OS}" \
	SLIVER_BOF_E2E_ARCH="${SLIVER_BOF_E2E_ARCH}" \
	bash --noprofile --norc -eo pipefail -c "${SLIVER_BOF_E2E_PREPARE_COMMAND}"
