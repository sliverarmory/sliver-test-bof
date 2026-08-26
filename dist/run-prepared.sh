#!/usr/bin/env bash

set -euo pipefail

: "${SLIVER_BOF_CONTROLLER:?prepared controller path is required}"
: "${SLIVER_BOF_PREPARED_FILE:?prepared acquisition descriptor is required}"
: "${SLIVER_BOF_STATE_FILE:?cleanup state file is required}"
: "${SLIVER_BOF_RESULTS_DIR:?results directory is required}"
: "${SLIVER_BOF_TARGET:?target is required}"

if [[ ! -f "${SLIVER_BOF_CONTROLLER}" || ! -x "${SLIVER_BOF_CONTROLLER}" ]]; then
	echo "prepared controller is not an executable regular file: ${SLIVER_BOF_CONTROLLER}" >&2
	exit 2
fi
if [[ ! -f "${SLIVER_BOF_PREPARED_FILE}" ]]; then
	echo "prepared acquisition descriptor is not a regular file: ${SLIVER_BOF_PREPARED_FILE}" >&2
	exit 2
fi

# Replace the runner shell with a fresh environment and the compiled Go
# controller. In particular, no action input, GitHub token, Actions runtime
# token, proxy, cloud credential, or Go build control reaches Sliver or a BOF.
exec env -i \
	PATH="${PATH}" \
	HOME="${HOME:-}" \
	USERPROFILE="${USERPROFILE:-}" \
	APPDATA="${APPDATA:-}" \
	LOCALAPPDATA="${LOCALAPPDATA:-}" \
	SYSTEMDRIVE="${SYSTEMDRIVE:-}" \
	SYSTEMROOT="${SYSTEMROOT:-}" \
	WINDIR="${WINDIR:-}" \
	COMSPEC="${COMSPEC:-}" \
	PATHEXT="${PATHEXT:-}" \
	TEMP="${TEMP:-}" \
	TMP="${TMP:-}" \
	TMPDIR="${TMPDIR:-}" \
	LANG="${LANG:-}" \
	LC_ALL="${LC_ALL:-}" \
	LC_CTYPE="${LC_CTYPE:-}" \
	TZ="${TZ:-}" \
	SSL_CERT_DIR="${SSL_CERT_DIR:-}" \
	SSL_CERT_FILE="${SSL_CERT_FILE:-}" \
	RUNNER_TEMP="${RUNNER_TEMP:-}" \
	GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-}" \
	GITHUB_STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-}" \
	"${SLIVER_BOF_CONTROLLER}" run \
	--manifest "${SLIVER_BOF_MANIFEST:-.github/sliver-bof-e2e.yml}" \
	--workspace "${GITHUB_WORKSPACE:-${PWD}}" \
	--results-dir "${SLIVER_BOF_RESULTS_DIR}" \
	--sliver-version "${SLIVER_BOF_SLIVER_VERSION:-}" \
	--target "${SLIVER_BOF_TARGET}" \
	--action-version "${SLIVER_BOF_ACTION_VERSION:-}" \
	--prepared "${SLIVER_BOF_PREPARED_FILE}" \
	--state-file "${SLIVER_BOF_STATE_FILE}"
