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

# Build the manager binary
FROM golang:1.21-alpine as builder
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
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X 'main.version=${OPERATOR_VERSION}'" \
     -a -o manager main.go

FROM arigaio/atlas:latest-alpine as atlas

FROM alpine:3.17.3
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=atlas /atlas .
RUN chmod +x /atlas
ENV ATLAS_NO_UPDATE_NOTIFIER=1
USER 65532:65532
ENTRYPOINT ["/manager"]
