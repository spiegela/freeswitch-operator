# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26
# The builder runs on the build machine's own platform and cross-compiles for the target (GOARCH below), so a
# multi-arch build needs no emulation for Go.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# The module and build caches are BuildKit cache mounts: they outlive the build, so a rebuild compiles only what
# changed (CI carries them between runs too; see test-e2e.yml).
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build all four in one step, sharing the build cache: only the first compiles the dependencies. GOARCH has no
# default, so the binaries are built for the target platform buildx passes (the build machine's own for a plain
# docker build).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    export CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
 && go build -o manager cmd/main.go \
 && go build -o freeswitch-exporter ./cmd/freeswitch-exporter \
 && go build -o address-agent ./cmd/address-agent \
 && go build -o call-tracer ./cmd/call-tracer

# The FreeSWITCH exporter, a separate image (docker build --target exporter) so that a new operator release
# doesn't change the FreeSWITCH pod template, which would roll every fleet.
FROM gcr.io/distroless/static:nonroot AS exporter
WORKDIR /
COPY --from=builder /workspace/freeswitch-exporter .
USER 65532:65532
ENTRYPOINT ["/freeswitch-exporter"]

# The call tracer, its own image (docker build --target call-tracer): it runs apart from the edge, wherever the
# HEP copies and LiveKit's webhooks can reach it.
FROM gcr.io/distroless/static:nonroot AS call-tracer
WORKDIR /
COPY --from=builder /workspace/call-tracer .
USER 65532:65532
ENTRYPOINT ["/call-tracer"]

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
# It stays the last stage, so a plain docker build makes the manager.
FROM gcr.io/distroless/static:nonroot AS manager
WORKDIR /
COPY --from=builder /workspace/manager .
# The address agent ships in the manager's image, so the two move together; only its DaemonSet runs it.
COPY --from=builder /workspace/address-agent .
USER 65532:65532

ENTRYPOINT ["/manager"]
