# syntax = docker/dockerfile:1.4.1
# Copyright 2023 The Atlas Operator Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the manager binary.
# Pinned to BUILDPLATFORM so the compiler always runs natively and
# cross-compiles to TARGETARCH, instead of running under QEMU emulation.
FROM --platform=${BUILDPLATFORM} golang:1.26.6-alpine3.24 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG OPERATOR_VERSION

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
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
    go build -ldflags "-X 'main.version=${OPERATOR_VERSION}'" \
    -o manager cmd/main.go

# Fetching the Atlas CLI is a plain download, so this stage also runs on
# BUILDPLATFORM. The target architecture is passed to the install script
# explicitly, since it would otherwise be detected from the (emulated) host.
FROM --platform=${BUILDPLATFORM} alpine:3.23 as atlas
RUN apk add --no-cache curl
ARG TARGETARCH
ARG ATLAS_VERSION=extended-latest
ENV ATLAS_VERSION=${ATLAS_VERSION}
RUN curl -sSf https://atlasgo.sh | sh -s -- --platform "linux-${TARGETARCH}" -y

FROM alpine:3.23
RUN apk add --no-cache libcrypto3=3.5.8-r0 libssl3=3.5.8-r0
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=atlas --chmod=755 /usr/local/bin/atlas /usr/local/bin
ENV ATLAS_KUBERNETES_OPERATOR=1
USER 65532:65532
ENTRYPOINT ["/manager"]
