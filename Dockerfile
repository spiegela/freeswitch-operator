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
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o freeswitch-exporter ./cmd/freeswitch-exporter
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o address-agent ./cmd/address-agent

# The FreeSWITCH exporter, a separate image (docker build --target exporter) so that a new operator release
# doesn't change the FreeSWITCH pod template, which would roll every fleet.
FROM gcr.io/distroless/static:nonroot AS exporter
WORKDIR /
COPY --from=builder /workspace/freeswitch-exporter .
USER 65532:65532
ENTRYPOINT ["/freeswitch-exporter"]

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
