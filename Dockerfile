# syntax = docker/dockerfile:1.4.1
# Build the manager binary
FROM golang:1.20-alpine as builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager main.go

FROM arigaio/atlas:latest-alpine as atlas

FROM alpine:3.17.3
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=atlas /atlas .
RUN chmod +x /atlas
ENV ATLAS_NO_UPDATE_NOTIFIER=1
USER 65532:65532
ENTRYPOINT ["/manager"]
