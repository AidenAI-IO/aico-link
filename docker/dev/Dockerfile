# BASE_IMAGE lets users build through a registry mirror, for example
# docker.1ms.run/library/debian:trixie-slim@sha256:<same digest>.
ARG BASE_IMAGE=debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

# The agent is pure Go, so it is cross-compiled on the build platform instead
# of running the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS agent-builder

ARG DEBIAN_FRONTEND=noninteractive
ARG BUILDARCH
ARG GO_VERSION=1.26.0
ARG GO_DOWNLOAD_BASE=https://go.dev/dl

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl tar; \
    rm -rf /var/lib/apt/lists/*; \
    go_architecture="${BUILDARCH:-$(dpkg --print-architecture)}"; \
    case "$go_architecture" in \
        amd64) go_sha256="aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235" ;; \
        arm64) go_sha256="bd03b743eb6eb4193ea3c3fd3956546bf0e3ca5b7076c8226334afe6b75704cd" ;; \
        *) echo "Unsupported Go build architecture: $go_architecture (supported: amd64, arm64)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 --connect-timeout 20 \
        "${GO_DOWNLOAD_BASE%/}/go${GO_VERSION}.linux-${go_architecture}.tar.gz" \
        -o /tmp/go.tar.gz; \
    echo "${go_sha256}  /tmp/go.tar.gz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tar.gz; \
    rm /tmp/go.tar.gz

ENV PATH=/usr/local/go/bin:${PATH} GOTOOLCHAIN=local
WORKDIR /build

# GOPROXY is optional; unset or empty keeps Go's default
# (https://proxy.golang.org,direct). Example mirror: https://goproxy.cn,direct.
ARG GOPROXY
COPY src/agent/go.mod src/agent/go.sum ./
RUN go mod download
COPY src/agent/ ./
# Declared late so that a new version or commit only rebuilds the final step.
ARG TARGETARCH
ARG AICO_VERSION=dev
ARG AICO_COMMIT=unknown
RUN set -eux; \
    mkdir -p /out; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
        go build -trimpath \
        -ldflags="-s -w -X aico-agent/internal/agent.buildVersion=${AICO_VERSION} -X aico-agent/internal/agent.buildCommit=${AICO_COMMIT}" \
        -o /out/agent ./cmd/daemon; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
        go build -trimpath -ldflags="-s -w" -o /out/device-cli ./cmd/device-cli

FROM ${BASE_IMAGE} AS ttyd-builder

ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && set -eux; \
        ttyd_version="1.7.3"; \
        architecture="${TARGETARCH:-$(dpkg --print-architecture)}"; \
        case "$architecture" in \
            amd64) \
                asset="ttyd.x86_64"; \
                sha256="67cf926ddf157dcb089106c2aa6623b8613bb7f3b493048eeb6cc34423570b3c"; \
                ;; \
            arm64) \
                asset="ttyd.aarch64"; \
                sha256="c77fbb475a767c8f2d52c1fd28f167eb115a3b10ff114920d38c13f8ad4fcfbd"; \
                ;; \
            arm) \
                asset="ttyd.armhf"; \
                sha256="b0784080bd78f0a5916462672f461542c607f8ea7cee56b075e8cd04e1ffcc4d"; \
                ;; \
            *) echo "Unsupported ttyd Docker architecture: $architecture" >&2; exit 1 ;; \
        esac; \
        mkdir -p /out; \
        curl -fsSL --retry 3 \
            "https://github.com/tsl0922/ttyd/releases/download/${ttyd_version}/${asset}" \
            -o /out/ttyd; \
        echo "$sha256  /out/ttyd" | sha256sum -c -; \
        chmod 0755 /out/ttyd

FROM --platform=$BUILDPLATFORM node:22.23.2-bookworm-slim@sha256:48e4b67d85f87bd551df43704e24d252f56cc5f8e9718841aace50f19948f0f9 AS config-web-builder
WORKDIR /web
COPY src/config_web/package.json src/config_web/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY src/config_web/ ./
RUN npm run typecheck && npm run build

FROM ${BASE_IMAGE} AS runtime-base

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        python3 \
        python3-pip \
        util-linux \
    && rm -rf /var/lib/apt/lists/*

FROM runtime-base AS runtime-tools-builder

ARG TARGETARCH

RUN set -eux; \
    architecture="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$architecture" in \
        amd64) \
            fq_sha256="41c277c59dffacfba9c9f9f4ad3a75bef591d341e40b9ac89fc85acbdf645fee"; \
            yq_sha256="fa52a4e758c63d38299163fbdd1edfb4c4963247918bf9c1c5d31d84789eded4"; \
            rg_target="x86_64-unknown-linux-musl"; \
            rg_sha256="33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c"; \
            ;; \
        arm64) \
            fq_sha256="217eba0d2cd03c8cbf9c54e45ecb9700b7100d592b71410ee3628c6d423cd328"; \
            yq_sha256="578648e463a11c1b6db6010cbf41eafed6bee79466fcffa1bb446672cf7945ea"; \
            rg_target="aarch64-unknown-linux-musl"; \
            rg_sha256="800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915"; \
            ;; \
        *) echo "Unsupported runtime tools architecture: $architecture" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out; \
    curl -fsSL --retry 3 \
        "https://github.com/wader/fq/releases/download/v0.17.0/fq_0.17.0_linux_${architecture}.tar.gz" \
        -o /tmp/fq.tar.gz; \
    echo "$fq_sha256  /tmp/fq.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/fq.tar.gz -C /out fq; \
    curl -fsSL --retry 3 \
        "https://github.com/mikefarah/yq/releases/download/v4.53.3/yq_linux_${architecture}" \
        -o /out/yq; \
    echo "$yq_sha256  /out/yq" | sha256sum -c -; \
    curl -fsSL --retry 3 \
        "https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/ripgrep-15.2.0-${rg_target}.tar.gz" \
        -o /tmp/rg.tar.gz; \
    echo "$rg_sha256  /tmp/rg.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/rg.tar.gz -C /tmp; \
    cp "/tmp/ripgrep-15.2.0-${rg_target}/rg" /out/rg; \
    chmod 0755 /out/fq /out/rg /out/yq

FROM runtime-base AS runtime

COPY --from=agent-builder /out/agent /usr/lib/aico/agent
COPY --from=agent-builder /out/device-cli /usr/local/bin/device-cli
COPY --from=ttyd-builder /out/ttyd /usr/local/bin/ttyd
COPY --from=runtime-tools-builder /out/fq /usr/bin/fq
COPY --from=runtime-tools-builder /out/rg /usr/bin/rg
COPY --from=runtime-tools-builder /out/yq /usr/bin/yq
COPY --from=config-web-builder /web/dist/ /usr/share/aico/config-web/
COPY src/agent/config/skills/ /usr/share/aico/skills/
COPY benchmark/ios_device/quick_actions.json /usr/share/aico/quick-actions/ios_device.json
COPY benchmark/iphone_mirroring/quick_actions.json /usr/share/aico/quick-actions/iphone_mirroring.json
COPY docker/dev/agent.toml /opt/aico/defaults/agent.toml
COPY docker/dev/aico-env-run /usr/local/bin/aico-env-run
COPY docker/dev/aico-quick-actions /usr/local/bin/aico-quick-actions
COPY docker/dev/aico-wifi-proxy-service /usr/local/bin/aico-wifi-proxy-service
COPY docker/dev/agent-service.sh /usr/local/bin/aico-agent-service
COPY docker/dev/entrypoint.sh /usr/local/bin/aico-docker-entrypoint

RUN chmod 0755 \
        /usr/lib/aico/agent \
        /usr/local/bin/aico-env-run \
        /usr/local/bin/aico-quick-actions \
        /usr/local/bin/aico-wifi-proxy-service \
        /usr/local/bin/aico-agent-service \
        /usr/local/bin/aico-docker-entrypoint \
        /usr/local/bin/ttyd

ENV AICO_AGENT_BIN=/usr/lib/aico/agent \
    AICO_AGENT_INIT_SCRIPT=/usr/local/bin/aico-agent-service \
    AICO_WIFI_PROXY_INIT_SCRIPT=/usr/local/bin/aico-wifi-proxy-service \
    AICO_ENV_RUN_BIN=/usr/local/bin/aico-env-run \
    AICO_BUNDLED_SKILLS_DIR=/usr/share/aico/skills \
    AICO_SYSTEM_ENV=/userdata/system/env \
    PYTHONUSERBASE=/userdata/agent/python \
    PIP_USER=1 \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    AICO_ENABLE_TERMINAL=0 \
    AICO_CLASSIC_VOICE=0

ARG AICO_VERSION=dev
ARG AICO_COMMIT=unknown

LABEL org.opencontainers.image.title="Aico Link" \
    org.opencontainers.image.source="https://github.com/AidenAI-IO/aico-link" \
    org.opencontainers.image.version="${AICO_VERSION}" \
    org.opencontainers.image.revision="${AICO_COMMIT}"

EXPOSE 80 8080
VOLUME ["/userdata"]

ENTRYPOINT ["/usr/local/bin/aico-docker-entrypoint"]
