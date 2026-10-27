# Build the manager binary.
# Base images are pinned by digest to make the build reproducible and to make
# supply-chain provenance explicit. To bump: pull the new tag and run
#   docker inspect <tag> --format '{{index .RepoDigests 0}}'
# then update both the digest and the human-readable tag comment below.
# golang:1.27.3 (refreshed 2026-10-27) — go1.27.3
FROM golang:1.27.3@sha256:921f61120011c774a7b37fd80bd3c3d259636090d3cd002b6a344322a0328082 as builder
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
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"] 