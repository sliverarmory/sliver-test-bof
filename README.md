# Sliver BOF end-to-end action

`sliver-test-bof` runs Beacon Object Files against real [Sliver](https://github.com/BishopFox/sliver) session and beacon implants. A test starts an isolated loopback Sliver server, creates a short-lived multiplayer operator profile, generates an HTTP implant, waits for its connection over gRPC, invokes the BOF, checks its output, and tears the environment down.

The repository exposes two interfaces:

- The reusable workflow plans and runs every `os/arch` suite on an appropriate GitHub-hosted runner.
- The root action runs one target in the caller's existing job, so a repository can own its build and matrix.

This is an initial implementation. Pin it to a full commit SHA in required CI and validate it on non-production BOFs before expanding its use.

The initial reusable workflow and direct action are GitHub.com-only. They use GitHub's `$/` self-repository action references; the workflow also uses exact `job.workflow_repository`/`job.workflow_sha` identity and the GitHub.com-only artifact action generations (`upload-artifact` v7 and `download-artifact` v8). They are not currently compatible with GitHub Enterprise Server.

## Reusable workflow

Add a manifest at `.github/sliver-bof-e2e.yml`, then call the workflow:

```yaml
name: BOF end-to-end

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  bof-e2e:
    uses: sliverarmory/sliver-test-bof/.github/workflows/e2e.yml@main
    with:
      manifest: .github/sliver-bof-e2e.yml
    secrets:
      github-token: ${{ secrets.GITHUB_TOKEN }}
```

Use `@main` only while evaluating the initial version. Replace it with a full action commit SHA for stable or required checks.

The planner validates the manifest, resolves the mutable Sliver selector once, and emits a dynamic matrix. Every matrix job therefore tests the same Sliver revision. Omitting the reusable workflow's `sliver-version` input makes `sliver.version` in the manifest authoritative; a non-empty workflow input overrides it. The workflow uses this allowlisted mapping:

| Implant target | GitHub-hosted runner |
|---|---|
| `darwin/amd64` | `macos-15-intel` |
| `darwin/arm64` | `macos-15` |
| `linux/386` | `ubuntu-24.04`; the test lifecycle runs in a full Debian i386 container through Docker/QEMU |
| `linux/amd64` | `ubuntu-24.04` |
| `linux/arm64` | `ubuntu-24.04-arm` |
| `windows/386` | `windows-2022` |
| `windows/amd64` | `windows-2022` |
| `windows/arm64` | `windows-11-arm` |

The manifest cannot supply arbitrary runner labels. That keeps pull-request-controlled configuration away from persistent self-hosted runners.

The reusable workflow plans immediately after checkout, so every referenced `.o` file must already exist in the checked-out revision. If BOFs are produced by an earlier build step, use the direct action in a caller-owned matrix (or commit/download the objects before invoking it).

## Manifest

The [v1 JSON Schema](schema/sliver-bof-e2e-v1.schema.json) is strict: unknown fields, duplicate names, unsupported targets, empty collection globs, invalid assertions, and files outside the checked-out workspace are rejected during planning.

```yaml
schema: sliver-bof-e2e/v1

sliver:
  version: master

defaults:
  bof_executor: reflektor
  entrypoint: go
  modes: [session, beacon]
  repeat: 1
  command_timeout: 2m

suites:
  - name: windows-amd64
    target:
      os: windows
      arch: amd64

    tests:
      - name: whoami
        object: build/windows/amd64/whoami.o
        args:
          - type: wstring
            value: example
        expect:
          output:
            contains: [UserName, SID]
            matches: ['(?m)^Privilege Name']

      - name: legacy-process-query
        object: build/windows/amd64/process.o
        bof_executor: coff-loader
        modes: [session]
        args:
          - type: int
            value: 1234
        expect:
          output:
            contains: [Process]

  - name: linux-arm64
    target:
      os: linux
      arch: arm64

    collections:
      - name: portable-smoke
        glob: build/linux/arm64/*.o
        expect:
          success_only: true
        overrides:
          env.o:
            expect:
              output:
                contains: ['PATH=']
```

All object and `file` argument paths are relative to the caller workspace. Symlinks that resolve outside it are rejected. A collection expands its glob into one test per object; overrides are keyed by object basename.

Settings may be placed on an individual test or collection. Collection overrides replace only the fields they name; an overridden `expect` replaces the whole inherited expectation, and `args` and `args_raw_base64` clear one another. The top-level `defaults` supports `bof_executor`, `entrypoint`, `modes`, `repeat`, and `command_timeout`.

### Arguments

Arguments are packed in the declared order using Beacon's little-endian argument format:

| Type | Value |
|---|---|
| `int` or `integer` | unsigned 32-bit integer |
| `short` | unsigned 16-bit integer |
| `string` | UTF-8 string |
| `wstring` | UTF-16LE string |
| `file` | workspace-relative file path; its bytes are packed |
| `bytes_base64` | base64-encoded bytes |

Use `args_raw_base64` instead of `args` when the caller already has a complete argument buffer. Typed and raw arguments are mutually exclusive.

### Output assertions

`expect.output` supports:

- `equals`
- `contains` and `not_contains`
- `matches` using Go/RE2 regular expressions
- `sha256`
- `equals_base64` for exact binary output

Use `expect.success_only: true` when the BOF has no stable content to assert. Otherwise at least one content assertion is required. Current Sliver Reflektor responses concatenate output records into one byte buffer, so assertions apply to that complete raw buffer rather than separate stdout and stderr streams.

## Executor selection

Executor selection is explicit per default, collection, or test. The action does not silently fall back between routes.

- `reflektor` uses Sliver's built-in BOF request (`CallExtensionReq.IsBOF`) and requires the connected implant to advertise the `bof_v1` capability. A missing capability is a preflight failure.
- `coff-loader` registers and calls the legacy COFFLoader extension. The built-in acquisition path is limited to supported Windows targets; incompatible target/loader combinations fail before execution.

Beacon invocations wait for the matching `beacon-taskresult`, fetch the completed task content, and decode the final extension response. Receipt of only the initial asynchronous RPC response is not considered a successful BOF run.

## Sliver selection

`sliver-version` accepts:

- `master`
- A full 40-character commit SHA
- `latest`
- A release tag such as `v1.7.3`

For both the direct action and reusable workflow, omitting `sliver-version` uses `sliver.version` from the manifest. A non-empty action or workflow input overrides the manifest. The reusable planner resolves that selection once and passes its immutable result to every target job. Release plans use an internal `release:<tag>@<commit>` selector so a moved tag cannot mix Sliver revisions across the matrix; manifests continue to use ordinary release tags.

`latest` and release tags download the exact host server asset and verify its minisign signature with Sliver's release public key. When GitHub publishes asset digests, those are checked too. `master` and commit selectors resolve to an immutable SHA and build the server from source with Sliver's server build tags. Job-private caches are keyed by the resolved release asset identity or source SHA, host platform, toolchain, and acquisition/build recipe—not by the mutable words `latest` or `master`.

The built-in `bof_v1` executor first appeared on Sliver's master branch. A release that predates it remains usable with `coff-loader`, but a manifest requesting `reflektor` will fail the capability preflight.

For direct single-target use, `sliver-server-path` bypasses acquisition and runs a caller-supplied server binary. Its SHA-256 is still recorded in the report.

## Direct action

An action cannot create jobs or choose runners, so callers using the root action own the matrix:

```yaml
jobs:
  bof-e2e:
    strategy:
      fail-fast: false
      matrix:
        include:
          - target: windows/amd64
            runner: windows-2022
          - target: linux/arm64
            runner: ubuntu-24.04-arm
    runs-on: ${{ matrix.runner }}
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v6
        with:
          persist-credentials: false
      - uses: actions/setup-go@v6
        with:
          go-version: 1.26.6
          cache: false
      - name: Build BOFs
        run: make bofs
      - name: Test BOFs
        uses: sliverarmory/sliver-test-bof@main
        with:
          manifest: .github/sliver-bof-e2e.yml
          target: ${{ matrix.target }}
          sliver-version: master
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

The root action's outputs are `results-dir` and `resolved-sliver`. Planning and aggregation are implementation details of the reusable workflow rather than public root-action modes.

For `linux/386`, prefer the reusable workflow. A direct-action caller must provide an equivalent complete i386 userspace and `/usr/bin/i686-linux-gnu-gcc`; binfmt/QEMU registration by itself is not enough for Reflektor BOFs that resolve libc or other system imports.

## Lifecycle and isolation

A target job stages its private server, client, implant, and result directories in a fresh subtree of `runner.temp`, separate from the checkout. It then:

1. Acquires or builds `sliver-server` and records its provenance.
2. Starts `sliver-server daemon` on a randomly selected loopback multiplayer port.
3. Generates a temporary all-permissions localhost operator profile and connects over mTLS/gRPC.
4. Starts an HTTP listener bound to loopback and generates session and/or beacon executables for the requested target.
5. Starts a fresh implant process for each test and mode, then correlates its connection event by implant name, PID, target, transport, and event cursor.
6. Invokes the BOF, waits for its final response, checks the configured assertions, and stops the tracked implant process group (or Windows Job Object).
7. Stops listeners and the server and removes private state.

Go cleanup handlers own normal teardown. An `always()` composite step launches the idempotent fallback cleanup program with a scrubbed environment: it terminates only exact process IDs recorded in the private state file and removes only recorded temporary paths under the runner's temp area. It never uses broad process-name matching. Operator profiles and private keys are never uploaded.

The Linux/386 row has an additional container boundary: its implant runs against a real 32-bit loader, libc, compiler, and system libraries, and removing the container catches descendants that detach from the implant process group. On native Darwin and Linux rows, a BOF-created child that deliberately calls `setsid`, changes process group, or daemonizes can escape the action's process-group cleanup and survive until the ephemeral runner is torn down. This action is a test harness, not containment for adversarial BOFs.

## Results

Every target writes:

- `report.json` using `sliver-bof-e2e-results/v1`
- `junit.xml`
- `summary.md`, also appended to the GitHub job summary
- server and per-implant logs, with bounded tails embedded in failure diagnostics
- raw BOF output for every invocation

Reports identify the action version, resolved Sliver selector/revision and server digest, stable expanded case ID, collection (when applicable), object digest, requested executor, mode, repeat number, timing, status, and failing lifecycle stage. Reusable-workflow reports use the exact repository and `job.workflow_sha`; direct-action reports are exact when the action is pinned to a full SHA and otherwise identify the requested mutable ref. The reusable workflow uploads each target separately and downloads all target reports into a clean `runner.temp` tree in an `always()` aggregate job. Aggregation requires the exact manifest-expanded inventory for every target, case ID, mode, and repeat; rejects missing, duplicate, unexpected, metadata-mismatched, or failed cases; and requires every report to agree on the action version and resolved Sliver provenance before uploading the combined tree for seven days.

## Security boundary

Generated Sliver implants and BOFs are native code executing as the runner user. Temporary directories, acquisition/runtime process separation, and sanitized subprocess environments are operational hygiene, not a sandbox. A short-lived JavaScript action builds the trusted controller and performs acquisition; it then exits. A separate shell step replaces its process image with the compiled controller under an explicit allowlisted environment, so the forwarded GitHub token, action inputs, Actions service tokens, proxies, cloud credentials, and Go build controls are absent while Sliver and BOFs execute. The Linux/386 route provides the equivalent boundary with a private token file followed by `exec env -i` inside the container.

This separation removes credentials explicitly forwarded to the action from the BOF execution tree. It cannot isolate malicious same-user native code from the GitHub Runner service itself or from unrelated job processes; environment scrubbing cannot make a shared operating-system account a security boundary.

Run this workflow on GitHub-hosted ephemeral runners with `contents: read`, no cloud/OIDC credentials, no unrelated repository secrets, and `persist-credentials: false`. Do not use it from `pull_request_target`. Do not execute unreviewed BOFs on persistent self-hosted runners. Both the multiplayer and HTTP listeners are hard-bound to loopback, but that does not make native code safe.
