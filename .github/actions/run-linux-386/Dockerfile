# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS controller-build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=sliver-bof-e2e-controller-mod,target=/go/pkg/mod,sharing=locked \
	go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,id=sliver-bof-e2e-controller-build,target=/root/.cache/go-build,sharing=locked \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -buildvcs=false -trimpath -tags client,go_sqlite \
	-o /out/sliver-bof-e2e ./cmd/sliver-bof-e2e

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS go-toolchain

FROM --platform=$TARGETPLATFORM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

RUN apt-get update \
	&& apt-get install -y --no-install-recommends build-essential ca-certificates git \
	&& rm -rf /var/lib/apt/lists/*

# The action controller and Sliver server are static amd64 binaries running on
# the amd64 host kernel. The implant and native Reflektor loader execute against
# this image's real i386 loader, libc, compiler, and system libraries.
COPY --from=go-toolchain /usr/local/go /usr/local/go
COPY --from=controller-build /out/sliver-bof-e2e /usr/local/bin/sliver-bof-e2e
COPY .github/actions/run-linux-386/entrypoint.sh /usr/local/lib/sliver-bof-e2e/entrypoint.sh

ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
	SLIVER_LINUX_CC_32=/usr/bin/i686-linux-gnu-gcc

ENTRYPOINT ["/bin/sh", "/usr/local/lib/sliver-bof-e2e/entrypoint.sh"]
