ARG TEST_PLATFORM=linux/amd64
FROM --platform=${TEST_PLATFORM} debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

ARG DEBIAN_FRONTEND=noninteractive
ARG GO_VERSION=1.26.0
ARG GO_SHA256=aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235
ARG UV_VERSION=0.8.14
ARG NODE_VERSION=v22.23.2
ARG NODE_SHA256=d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307

COPY docker/test/debian.sources /etc/apt/sources.list.d/debian.sources

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        binutils-arm-linux-gnueabihf \
        binutils \
        build-essential \
        ca-certificates \
        cmake \
        curl \
        device-tree-compiler \
        docker-cli \
        docker-compose \
        dnsmasq-base \
        e2fsprogs \
        file \
        g++-arm-linux-gnueabihf \
        gcc-arm-linux-gnueabihf \
        git \
        gnupg \
        gpgv \
        jq \
        libopencv-core-dev \
        libopencv-imgproc-dev \
        ninja-build \
        openssl \
        patch \
        python3 \
        python3-pip \
        python3-venv \
        python3-yaml \
        ripgrep \
        ruby \
        shellcheck \
        systemd \
        tar \
        xz-utils \
        zstd \
    && rm -rf /var/lib/apt/lists/*

# The Docker sandbox smoke test runs in this image, not on the host.
RUN mkdir -p /usr/local/lib/docker/cli-plugins \
    && ln -s /usr/bin/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose \
    && docker compose version

RUN curl -fsSL --retry 3 --connect-timeout 20 \
        "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
        -o /tmp/go.tar.gz \
    && echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

RUN curl -fsSL --retry 3 --connect-timeout 20 \
        "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-x64.tar.xz" \
        -o /tmp/node.tar.xz \
    && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
    && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 \
    && rm /tmp/node.tar.xz

RUN python3 -m venv --system-site-packages /opt/uv \
    && /opt/uv/bin/pip install --no-cache-dir "uv==${UV_VERSION}" \
    && ln -s /opt/uv/bin/uv /usr/local/bin/uv

ENV PATH=/usr/local/go/bin:/opt/uv/bin:${PATH} \
    GOTOOLCHAIN=local \
    GONOSUMDB= \
    GOPROXY=https://proxy.golang.org|direct \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    UV_CACHE_DIR=/tmp/uv-cache \
    UV_LINK_MODE=copy

WORKDIR /workspace

RUN mkdir -p /tmp/aico-home /tmp/aico-cache \
    && chmod 1777 /tmp/aico-home /tmp/aico-cache \
    && test "$(go env GOVERSION)" = "go${GO_VERSION}" \
    && node --version | grep -Eq '^v22\.' \
    && cmake --version \
    && uv --version
