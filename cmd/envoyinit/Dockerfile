ARG ENVOY_IMAGE
ARG CARGO_ZBUILD_IMAGE=ghcr.io/rust-cross/cargo-zigbuild:0.19.8
# Stage 1: pull in all rust build dependency and cache it in docker build layer cache
FROM --platform=$TARGETPLATFORM ${CARGO_ZBUILD_IMAGE} AS rust_build_deps
WORKDIR /build
RUN apt update && apt install -y clang
ARG RUSTFORMATIONS_DIR=./rustformations
# Unfortunately, there is no easy way to find all the Cargo.toml files and iterate
# through the directories in Dockerfile. If we add more local crate, will need to add
# it's Cargo.toml file below
COPY ${RUSTFORMATIONS_DIR}/Cargo.toml ${RUSTFORMATIONS_DIR}/Cargo.lock ./
RUN mkdir -p rustformations/src transformations/src \
    && echo "pub fn dummy() {}" > rustformations/src/lib.rs \
    && echo "pub fn dummy() {}" > transformations/src/lib.rs
COPY ${RUSTFORMATIONS_DIR}/rustformations/Cargo.toml ./rustformations
COPY ${RUSTFORMATIONS_DIR}/transformations/Cargo.toml ./transformations
# REMOVE-ENVOY-1.37 : after upgrading to envoy 1.37, we can remove these
ARG PATCHED_SDK_DIR=patched-envoy-sdk
COPY ${RUSTFORMATIONS_DIR}/${PATCHED_SDK_DIR} ${PATCHED_SDK_DIR}
# END REMOVE
# Derive RUST_BUILD_ARCH from TARGETPLATFORM
# TARGETPLATFORM is like "linux/amd64" or "linux/arm64"
ARG TARGETPLATFORM
RUN if [ "$TARGETPLATFORM" = "linux/arm64" ]; then \
    echo "aarch64" > /tmp/rust_arch; \
    else \
    echo "x86_64" > /tmp/rust_arch; \
    fi
# Because we built the dummy lib in this Stage to cache all the dependencies in docker layer cache
# That unfortunately also cached the dummy libs, and in Stage 2 when we COPY, it preserved
# the timestamps of the source code, so it never re-compile the real lib. Need to remove them
# here
RUN RUST_ARCH=$(cat /tmp/rust_arch) && \
    cargo fetch && \
    cargo zigbuild --target ${RUST_ARCH}-unknown-linux-gnu && \
    find /build/target \( -name librust_module.so -o -name 'libtransformations*.rlib' \) -type f -delete

# Stage 2: build envoy dynamic module
FROM --platform=$TARGETPLATFORM ${CARGO_ZBUILD_IMAGE} AS rust_builder
WORKDIR /build
RUN apt update && apt install -y clang

# Copy cached target and cargo dirs from rust_build_deps from step 1
COPY --from=rust_build_deps /usr/local/cargo /usr/local/cargo
COPY --from=rust_build_deps /build/target /build/target
COPY --from=rust_build_deps /tmp/rust_arch /tmp/rust_arch

# Copy the source code and build the module.
ARG RUSTFORMATIONS_DIR=./rustformations
COPY ${RUSTFORMATIONS_DIR} .
RUN RUST_ARCH=$(cat /tmp/rust_arch) && \
    cargo zigbuild --target ${RUST_ARCH}-unknown-linux-gnu && \
    cp /build/target/${RUST_ARCH}-unknown-linux-gnu/debug/librust_module.so /build/librust_module.so

# Stage 3: build the final envoy wrapper image
FROM --platform=$TARGETPLATFORM ${ENVOY_IMAGE} AS envoy
ENV DEBIAN_FRONTEND=noninteractive

# Update our deps to make cve toil lower
# install wget for our default probes
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install wget ca-certificates -y \
    && rm -rf  /var/log/*log /var/lib/apt/lists/* /var/log/apt/* /var/lib/dpkg/*-old /var/cache/debconf/*-old

ARG TARGETARCH
COPY *envoyinit-linux-$TARGETARCH /usr/local/bin/envoyinit

ENV ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/usr/local/lib
COPY --from=rust_builder /build/librust_module.so /usr/local/lib/librust_module.so

# SDS-specific setup, only used if ENVOY_SIDECAR=true
ARG ENTRYPOINT_SCRIPT=/docker-entrypoint.sh
COPY $ENTRYPOINT_SCRIPT /

USER 10101

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD []
