# Build the manager binary.
# Base images are pinned by digest to make the build reproducible and to make
# supply-chain provenance explicit. To bump: pull the new tag and run
#   docker inspect <tag> --format '{{index .RepoDigests 0}}'
# then update both the digest and the human-readable tag comment below.
# golang:1.27 (refreshed 2026-09-10) — go1.27.1
FROM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea as builder
WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don'''t need to re-download as much
# and so that source changes don'''t invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY controllers/ controllers/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build on a Mac M1 it will generate an image with arch arm64.
# More info: https://doc.crds.dev/reference/go/go-env
ARG TARGETOS
ARG TARGETARCH
# Build the package, not a single file — multi-file packages
# (cmd/manager/{main,flags}.go) require ./cmd/manager so all sources
# in the package are picked up.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager ./cmd/manager

# Use distroless as minimal base image to package the manager binary.
# Refer to https://github.com/GoogleContainerTools/distroless for more details.
# gcr.io/distroless/static:nonroot (refreshed 2026-05-03) — see digest-update
# instructions in the builder stage above.
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"] 