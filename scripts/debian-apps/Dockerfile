FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

ARG DEBIAN_FRONTEND=noninteractive

COPY scripts/debian-apps/debian.sources /etc/apt/sources.list.d/debian.sources

RUN dpkg --add-architecture armhf \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        autoconf \
        automake \
        binutils-arm-linux-gnueabihf \
        binutils \
        ca-certificates \
        cmake \
        curl \
        file \
        g++-arm-linux-gnueabihf \
        gcc-arm-linux-gnueabihf \
        git \
        flite1-dev:armhf \
        libmpg123-dev:armhf \
        libsndfile1-dev:armhf \
        libsonic-dev:armhf \
        libtool \
        libutfcpp-dev \
        make \
        ninja-build \
        patch \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*
