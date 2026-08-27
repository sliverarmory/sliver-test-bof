#!/bin/sh

set -eu

if [ "${SLIVER_BOF_TARGET:-}" != "linux/386" ]; then
	echo "linux/386 container received unsupported target: ${SLIVER_BOF_TARGET:-}" >&2
	exit 2
fi

manifest=${SLIVER_BOF_MANIFEST:-.github/sliver-bof-e2e.yml}
results_dir=${SLIVER_BOF_RESULTS_DIR:?results directory is required}
prepared_dir="${HOME}/prepared"
prepared_file="${prepared_dir}/acquisition.json"
state_file="${HOME}/cleanup-state.json"
token_file=${SLIVER_BOF_GITHUB_TOKEN_FILE:?GitHub token handoff file is required}

umask 077
mkdir -p "${prepared_dir}" "${results_dir}"
if [ ! -f "${token_file}" ]; then
	echo "GitHub token handoff is not a regular file" >&2
	exit 2
fi
github_token=$(cat "${token_file}")
rm -f -- "${token_file}"

# Only this short-lived acquisition subprocess receives the GitHub token.
# The entrypoint is then replaced with a fresh, token-free process image before
# Sliver or an implant starts.
env -i \
	PATH="${PATH}" \
	HOME="${HOME}" \
	RUNNER_TEMP="${RUNNER_TEMP}" \
	TMPDIR="${HOME}" \
	LANG=C.UTF-8 \
	GITHUB_TOKEN="${github_token}" \
	GITHUB_OUTPUT="${GITHUB_OUTPUT:-}" \
	GITHUB_STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-}" \
	/usr/local/bin/sliver-bof-e2e prepare \
	--manifest "${manifest}" \
	--workspace "${GITHUB_WORKSPACE}" \
	--object-root "${SLIVER_BOF_OBJECT_ROOT}" \
	--results-dir "${results_dir}" \
	--github-output "${GITHUB_OUTPUT:-}" \
	--sliver-version "${SLIVER_BOF_SLIVER_VERSION}" \
	--target "${SLIVER_BOF_TARGET}" \
	--action-version "${SLIVER_BOF_ACTION_VERSION}" \
	--prepared "${prepared_file}"

github_token=
unset github_token

exec env -i \
	PATH="${PATH}" \
	HOME="${HOME}" \
	RUNNER_TEMP="${RUNNER_TEMP}" \
	TMPDIR="${HOME}" \
	LANG=C.UTF-8 \
	GITHUB_STEP_SUMMARY="${GITHUB_STEP_SUMMARY:-}" \
	/usr/local/bin/sliver-bof-e2e run \
	--manifest "${manifest}" \
	--workspace "${GITHUB_WORKSPACE}" \
	--object-root "${SLIVER_BOF_OBJECT_ROOT}" \
	--results-dir "${results_dir}" \
	--sliver-version "${SLIVER_BOF_SLIVER_VERSION}" \
	--target "${SLIVER_BOF_TARGET}" \
	--action-version "${SLIVER_BOF_ACTION_VERSION}" \
	--prepared "${prepared_file}" \
	--state-file "${state_file}"
