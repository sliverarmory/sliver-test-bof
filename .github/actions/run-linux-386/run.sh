#!/usr/bin/env bash

set -euo pipefail

if [[ "${SLIVER_BOF_TARGET}" != "linux/386" ]]; then
  echo "linux/386 container action received unsupported target: ${SLIVER_BOF_TARGET}" >&2
  exit 2
fi

action_root="$(cd "${GITHUB_ACTION_PATH}/../../.." && pwd -P)"
workspace="$(cd "${GITHUB_WORKSPACE}" && pwd -P)"
runner_temp="$(cd "${RUNNER_TEMP}" && pwd -P)"
unique="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-${RANDOM}"
image_name="sliver-bof-e2e-linux-386:${unique}"
container_name="sliver-bof-e2e-linux-386-${unique}"
identity_container_name="${container_name}-identity"
container_home="${runner_temp}/sliver-bof-e2e-linux-386-home-${unique}"
token_file="${container_home}/github-token"
passwd_file="${container_home}/passwd"
group_file="${container_home}/group"

if [[ "${runner_temp}" == "/" || "${container_home}" != "${runner_temp}"/sliver-bof-e2e-linux-386-home-* ]]; then
  echo "refusing unsafe Linux/386 container home: ${container_home}" >&2
  exit 2
fi

umask 077
mkdir -p "${container_home}"
chmod 700 "${container_home}"
printf '%s' "${SLIVER_BOF_GITHUB_TOKEN:-}" >"${token_file}"
unset SLIVER_BOF_GITHUB_TOKEN

cleanup() {
  docker rm --force "${identity_container_name}" >/dev/null 2>&1 || true
  docker rm --force "${container_name}" >/dev/null 2>&1 || true
  docker image rm --force "${image_name}" >/dev/null 2>&1 || true
  rm -rf -- "${container_home}"
}
trap cleanup EXIT

docker buildx build \
  --load \
  --platform linux/386 \
  --file "${GITHUB_ACTION_PATH}/Dockerfile" \
  --tag "${image_name}" \
  "${action_root}"

# Preserve the caller's numeric identity and make libc/getpwuid/os/user lookups
# meaningful inside the i386 rootfs. docker cp creates these files as the host
# user, so they remain writable here and readable after the read-only mounts.
docker create \
  --name "${identity_container_name}" \
  --platform linux/386 \
  --entrypoint /bin/true \
  "${image_name}" >/dev/null
docker cp "${identity_container_name}:/etc/passwd" "${passwd_file}"
docker cp "${identity_container_name}:/etc/group" "${group_file}"
docker rm "${identity_container_name}" >/dev/null

host_uid="$(id -u)"
host_gid="$(id -g)"
host_user="$(id -un)"
host_group="$(id -gn)"
if [[ ! "${host_user}" =~ ^[A-Za-z_][A-Za-z0-9_.-]*$ ]]; then
  host_user="sliverbof${host_uid}"
fi
if [[ ! "${host_group}" =~ ^[A-Za-z_][A-Za-z0-9_.-]*$ ]]; then
  host_group="sliverbof${host_gid}"
fi
if awk -F: -v name="${host_user}" '$1 == name { found = 1 } END { exit !found }' "${passwd_file}"; then
  host_user="sliverbof${host_uid}"
fi
if awk -F: -v name="${host_group}" '$1 == name { found = 1 } END { exit !found }' "${group_file}"; then
  host_group="sliverbof${host_gid}"
fi
if ! awk -F: -v id="${host_gid}" '$3 == id { found = 1 } END { exit !found }' "${group_file}"; then
  printf '%s:x:%s:\n' "${host_group}" "${host_gid}" >>"${group_file}"
fi
if ! awk -F: -v id="${host_uid}" '$3 == id { found = 1 } END { exit !found }' "${passwd_file}"; then
  printf '%s:x:%s:%s:Sliver BOF E2E:%s:/bin/sh\n' \
    "${host_user}" "${host_uid}" "${host_gid}" "${container_home}" >>"${passwd_file}"
fi
chmod 644 "${passwd_file}" "${group_file}"

docker_arguments=(
  run --rm --init
  --name "${container_name}"
  --platform linux/386
  --user "$(id -u):$(id -g)"
  --cap-drop ALL
  --security-opt no-new-privileges
  --volume "${workspace}:${workspace}"
  --volume "${runner_temp}:${runner_temp}"
  --volume "${passwd_file}:/etc/passwd:ro"
  --volume "${group_file}:/etc/group:ro"
  --env "HOME=${container_home}"
  --env "GITHUB_WORKSPACE=${workspace}"
  --env "RUNNER_TEMP=${runner_temp}"
  --env "GITHUB_OUTPUT=${GITHUB_OUTPUT:-}"
  --env "GITHUB_STEP_SUMMARY=${GITHUB_STEP_SUMMARY:-}"
  --env "SLIVER_BOF_MANIFEST=${SLIVER_BOF_MANIFEST}"
  --env "SLIVER_BOF_TARGET=${SLIVER_BOF_TARGET}"
  --env "SLIVER_BOF_OBJECT_ROOT=${SLIVER_BOF_OBJECT_ROOT}"
  --env "SLIVER_BOF_SLIVER_VERSION=${SLIVER_BOF_SLIVER_VERSION}"
  --env "SLIVER_BOF_RESULTS_DIR=${SLIVER_BOF_RESULTS_DIR}"
  --env "SLIVER_BOF_GITHUB_TOKEN_FILE=${token_file}"
  --env "SLIVER_BOF_ACTION_VERSION=${SLIVER_BOF_ACTION_VERSION}"
)

docker "${docker_arguments[@]}" "${image_name}"
