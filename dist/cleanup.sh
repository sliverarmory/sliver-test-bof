#!/usr/bin/env bash

set -euo pipefail

if [[ -z "${SLIVER_BOF_NODE:-}" || ! -x "${SLIVER_BOF_NODE}" ]]; then
	echo "::warning::Prepared cleanup runtime is unavailable; relying on the ephemeral runner teardown"
	exit 0
fi
if [[ -z "${SLIVER_BOF_ACTION_ROOT:-}" || ! -f "${SLIVER_BOF_ACTION_ROOT}/dist/post.js" ]]; then
	echo "::warning::Cleanup program is unavailable; relying on the ephemeral runner teardown"
	exit 0
fi

# The cleanup program is launched as an ordinary, explicitly scrubbed process
# rather than a JavaScript action post hook, so it never receives action inputs
# or the runner's automatic Actions service tokens.
exec env -i \
	PATH="${PATH}" \
	HOME="${HOME:-}" \
	USERPROFILE="${USERPROFILE:-}" \
	SYSTEMROOT="${SYSTEMROOT:-}" \
	WINDIR="${WINDIR:-}" \
	COMSPEC="${COMSPEC:-}" \
	PATHEXT="${PATHEXT:-}" \
	TEMP="${TEMP:-}" \
	TMP="${TMP:-}" \
	TMPDIR="${TMPDIR:-}" \
	RUNNER_TEMP="${RUNNER_TEMP:-}" \
	GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-}" \
	STATE_state_file="${SLIVER_BOF_STATE_FILE:-}" \
	STATE_prepared_dir="${SLIVER_BOF_PREPARED_DIR:-}" \
	"${SLIVER_BOF_NODE}" "${SLIVER_BOF_ACTION_ROOT}/dist/post.js"
