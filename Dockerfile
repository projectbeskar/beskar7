# Build the manager binary.
# Base images are pinned by digest to make the build reproducible and to make
# supply-chain provenance explicit. To bump: pull the new tag and run
#   docker inspect <tag> --format '{{index .RepoDigests 0}}'
# then update both the digest and the human-readable tag comment below.
# golang:1.27.4 (refreshed 2026-11-14) — go1.27.4
FROM golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d as builder
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
FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"] 