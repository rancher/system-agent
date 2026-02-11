# syntax=docker/dockerfile:1.4

# Copyright © 2025 SUSE LLC
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

# Build the rancher-system-agent binary
# This Dockerfile replaces Dockerfile.dapper for containerized builds

ARG GOLANG_VERSION=1.24
ARG builder_image=registry.suse.com/bci/golang:${GOLANG_VERSION}

# =============================================================================
# Builder stage - compiles the Go binary
# =============================================================================
FROM --platform=$BUILDPLATFORM ${builder_image} AS builder

WORKDIR /workspace

# Run this with docker build --build-arg goproxy=$(go env GOPROXY) to override the goproxy
ARG goproxy=https://proxy.golang.org
ENV GOPROXY=${goproxy}

# Cache dependencies - copy go.mod and go.sum first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the rest of the source code
COPY ./ ./

# Build arguments for version injection
ARG TARGETOS
ARG TARGETARCH
ARG ldflags=""
ARG VERSION=""
ARG COMMIT=""

# Build the binary
# Do not force rebuild of up-to-date packages (do not use -a) and use the compiler cache folder
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
    -ldflags "${ldflags} -extldflags '-static' -s -X github.com/rancher/system-agent/pkg/version.Version=${VERSION} -X github.com/rancher/system-agent/pkg/version.GitCommit=${COMMIT}" \
    -o rancher-system-agent .

# =============================================================================
# Final stage for system-agent image (minimal runtime)
# =============================================================================
FROM registry.suse.com/bci/bci-micro:15.6 AS runtime-base

# Temporary build stage image for installing packages
FROM registry.suse.com/bci/bci-base:15.6 AS runtime-builder

# Install system packages using builder image that has zypper
COPY --from=runtime-base / /chroot/

RUN zypper refresh && \
    zypper --installroot /chroot -n in --no-recommends \
    openssl patterns-base-fips && \
    zypper --installroot /chroot clean -a && \
    rm -rf /chroot/var/cache/zypp/* /chroot/var/log/zypp/* /chroot/tmp/* /chroot/var/tmp/* /chroot/usr/share/doc/packages/*

# =============================================================================
# system-agent - minimal runtime image
# =============================================================================
FROM runtime-base AS system-agent

LABEL org.opencontainers.image.source=https://github.com/rancher/system-agent
LABEL org.opencontainers.image.description="Rancher System Agent"

# Copy binaries and configuration files from runtime-builder to micro
COPY --from=runtime-builder /chroot/ /

# Copy the binary from builder stage
COPY --from=builder /workspace/rancher-system-agent /usr/bin/rancher-system-agent
RUN chmod +x /usr/bin/rancher-system-agent

CMD ["rancher-system-agent"]

# =============================================================================
# system-agent-suc - for system-upgrade-controller
# =============================================================================
# Temporary build stage for SUC packages
FROM registry.suse.com/bci/bci-base:15.6 AS suc-builder

ARG KUBECTL_VERSION=v1.34.1

# https://dl.k8s.io/release/v1.34.1/bin/linux/arm64/kubectl.sha256
ENV KUBECTL_SUM_arm64=420e6110e3ba7ee5a3927b5af868d18df17aae36b720529ffa4e9e945aa95450
# https://dl.k8s.io/release/v1.34.1/bin/linux/amd64/kubectl.sha256
ENV KUBECTL_SUM_amd64=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3

# Install system packages using builder image that has zypper
COPY --from=runtime-base / /chroot/

RUN zypper refresh && \
    zypper --installroot /chroot -n in --no-recommends \
    openssl patterns-base-fips grep && \
    zypper --installroot /chroot clean -a && \
    rm -rf /chroot/var/cache/zypp/* /chroot/var/log/zypp/* /chroot/tmp/* /chroot/var/tmp/* /chroot/usr/share/doc/packages/*

# Install curl in the builder stage (not in chroot) to download kubectl
RUN zypper in -y curl openssl

ARG TARGETARCH
RUN ARCH=${TARGETARCH:-amd64} && \
    if [ "$ARCH" = "amd64" ]; then KUBECTL_ARCH=amd64; fi && \
    if [ "$ARCH" = "arm64" ]; then KUBECTL_ARCH=arm64; fi && \
    curl -L -f -o kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KUBECTL_ARCH}/kubectl" && \
    KUBECTL_SUM="KUBECTL_SUM_${KUBECTL_ARCH}" && echo "${!KUBECTL_SUM}  kubectl" | sha256sum -c - && \
    install -o root -g root -m 0755 kubectl /chroot/usr/bin/kubectl && \
    rm kubectl

# =============================================================================
# system-agent-suc - final SUC runtime image
# =============================================================================
FROM runtime-base AS system-agent-suc

LABEL org.opencontainers.image.source=https://github.com/rancher/system-agent
LABEL org.opencontainers.image.description="Rancher System Agent for System Upgrade Controller"

# Copy binaries and configuration files from suc-builder to micro
COPY --from=suc-builder /chroot/ /

ENV CATTLE_AGENT_VAR_DIR="/var/lib/rancher/agent"

RUN mkdir -p /opt/rancher-system-agent-suc

# Copy scripts
COPY install.sh /opt/rancher-system-agent-suc/install.sh
COPY system-agent-uninstall.sh /opt/rancher-system-agent-suc/system-agent-uninstall.sh
COPY package/suc/run.sh /opt/rancher-system-agent-suc/run.sh

# Copy the binary from builder stage
COPY --from=builder /workspace/rancher-system-agent /opt/rancher-system-agent-suc/rancher-system-agent
RUN chmod +x /opt/rancher-system-agent-suc/rancher-system-agent

ENTRYPOINT ["/opt/rancher-system-agent-suc/run.sh"]
