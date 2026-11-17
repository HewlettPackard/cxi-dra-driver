# syntax=docker/dockerfile:1

# Production image for the CXI DRA driver.
#
# Self-contained: libcxi is built from public HPE Slingshot sources inside the
# builder stage, so nothing needs staging in bin/ beforehand and the build runs
# anywhere -- CI, a laptop, a Slingshot node -- for both arm64 and amd64.
#
# The pinned refs below MUST track the Slingshot Host Software release running
# on the target nodes. libcxi talks to the cxi kernel driver over a versioned
# ioctl ABI, so a userspace/kernel skew shows up as cxil_open_device or
# cxil_svc_* failures at runtime, not as a build error. Currently pinned to
# release/shs-14.0 (libcxi API 1.8.0 -- the first release carrying
# cxil_svc_set_netns / cxil_svc_get_netns, which internal/cxi/service_cgo.go
# depends on for netns-scoped services).

# Base images are build args so an air-gapped Slingshot site (or a laptop
# without Docker Hub credentials) can point them at an internal mirror, e.g.
#   --build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.26-bookworm
ARG GO_IMAGE=golang:1.26-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim

FROM ${GO_IMAGE} AS libcxi-build

# release/shs-14.0 tips as of 2026-09-07.
ARG CASSINI_HEADERS_REF=125b3d00d0d65029a21aa139f8dce88d845491c5
ARG CXI_DRIVER_REF=bb11c8594e4cffb90f83f687c6c339a5e2e67045
ARG LIBCXI_REF=136a213673e9e41c9229236b84a2cc6a013d278e

# Build deps from cray-libcxi.spec. libconfig/libuv/fuse/yaml/numa/sensors are
# only needed to satisfy configure's PKG_CHECK_MODULES -- they belong to the
# retry-handler and utils programs, which we don't build.
RUN apt-get update && apt-get install -y --no-install-recommends \
      autoconf automake libtool pkg-config \
      libnl-3-dev libnl-genl-3-dev \
      libconfig-dev libuv1-dev libfuse-dev libyaml-dev \
      libnuma-dev libsensors-dev \
 && rm -rf /var/lib/apt/lists/*

# libcxi's AM_CPPFLAGS resolves the two header sets as siblings of its own
# source tree: ../cxi-driver/include and ../cassini-headers/install/include.
# cassini-headers ships its headers pre-generated in include/, so the install/
# level that the vendor's Jenkins builds stage into is just a symlink here.
RUN set -eux; \
    fetch() { \
      mkdir -p "/shs/$1"; cd "/shs/$1"; \
      git init -q .; \
      git remote add origin "https://github.com/HewlettPackard/$2.git"; \
      git fetch -q --depth 1 origin "$3"; \
      git checkout -q FETCH_HEAD; \
    }; \
    fetch cassini-headers shs-cassini-headers "${CASSINI_HEADERS_REF}"; \
    fetch cxi-driver      shs-cxi-driver      "${CXI_DRIVER_REF}"; \
    fetch libcxi          shs-libcxi          "${LIBCXI_REF}"; \
    mkdir -p /shs/cassini-headers/install; \
    ln -s ../include /shs/cassini-headers/install/include

# Only src/libcxi.la is built: the utils and retry-handler targets pull in GPU
# runtimes and systemd and are not used by the driver. --without-systemd and
# the =no unit dirs keep configure from requiring systemd/udev pkg-config.
RUN set -eux; \
    cd /shs/libcxi; \
    ./autogen.sh; \
    ./configure \
      --prefix=/usr --libdir=/usr/lib64 \
      --without-systemd \
      --with-systemdsystemunitdir=no \
      --with-udevrulesdir=no; \
    make -j"$(nproc)" src/libcxi.la; \
    install -d /opt/libcxi/lib64 /opt/libcxi/include/libcxi; \
    cp -a src/.libs/libcxi.so* /opt/libcxi/lib64/; \
    cp -a include/libcxi.h /opt/libcxi/include/libcxi/

# libcxi.h is not self-contained: it pulls in "cxi_prov_hw.h",
# "uapi/misc/cxi.h" and "cassini_cntr_defs.h", and uapi/misc/cxi.h in turn
# pulls <uapi/ethernet/cxi-abi.h> -- which is where struct cxi_svc_desc is
# actually defined. Anything compiling against libcxi.h therefore needs both
# header sets on the include path, laid out the way the
# cray-cassini-headers-user and cray-cxi-driver-devel RPMs lay them out on a
# node: cassini flat in /usr/include, cxi-driver keeping its uapi/ and linux/
# subtrees.
RUN set -eux; \
    install -d /opt/shs-include; \
    cp -a /shs/cassini-headers/include/. /opt/shs-include/; \
    cp -a /shs/cxi-driver/include/.      /opt/shs-include/


FROM ${GO_IMAGE} AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
      libnl-3-dev \
 && rm -rf /var/lib/apt/lists/*

# Place libcxi exactly where the cgo directives in internal/cxi/service_cgo.go
# expect it (-I/usr/include, -L/usr/lib64 -lcxi) so this build and the
# documented on-node build compile identically, with no source divergence.
COPY --from=libcxi-build /opt/shs-include/ /usr/include/
COPY --from=libcxi-build /opt/libcxi/include/libcxi /usr/include/libcxi
COPY --from=libcxi-build /opt/libcxi/lib64/ /usr/lib64/

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# -buildvcs=false: the build context often has no usable .git (CI cache mounts,
# or a tree owned by another user on a Slingshot node, where git refuses VCS
# queries with "dubious ownership"). We don't need a VCS stamp.
RUN CGO_ENABLED=1 GOOS=linux go build -tags cxi -buildvcs=false \
      -o /cxi-dra-driver ./cmd/cxi-dra-driver

# Host-executed helpers must stay CGO-free: cxi-cdi-hook runs in the container
# runtime's namespace and slingshot-cni is exec'd by containerd, so neither can
# depend on this image's libc. The driver installs both onto the node at startup.
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o /cxi-cdi-hook ./cmd/cxi-cdi-hook \
 && CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o /slingshot-cni ./cmd/slingshot-cni


FROM ${RUNTIME_IMAGE}

RUN apt-get update && apt-get install -y --no-install-recommends \
      libnl-3-200 ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# /usr/lib64 is not on Debian's default search path; register it rather than
# relocating libcxi, so the runtime layout matches the node layout too.
COPY --from=libcxi-build /opt/libcxi/lib64/libcxi.so.1* /usr/lib64/
RUN echo /usr/lib64 > /etc/ld.so.conf.d/000-lib64.conf && ldconfig

# Binary stays at / to match the explicit `command: [/cxi-dra-driver]` in
# deploy/kubernetes/daemonset.yaml and the Helm chart's workload.yaml, which
# override the entrypoint. Moving it would CrashLoop every existing install.
COPY --from=builder /cxi-dra-driver /cxi-dra-driver
COPY --from=builder /cxi-cdi-hook /cxi-cdi-hook
COPY --from=builder /slingshot-cni /slingshot-cni

# The driver installs this ldconfig, dynamic linker and libc onto the node at
# startup (see internal/cdi.InstallHook) so cxi-cdi-hook never depends on the
# node's own glibc. The multiarch globs resolve to one file per architecture;
# the arch-neutral names match the --cdi-ldconfig-*-src defaults.
RUN set -eux; \
    mkdir -p /ldconfig-runtime; \
    set -- /lib/*-linux-gnu/libc.so.6;      cp -L "$1" /ldconfig-runtime/libc.so.6; \
    set -- /lib/*-linux-gnu/ld-linux-*.so.*; cp -L "$1" /ldconfig-runtime/ld.so; \
    cp -L /sbin/ldconfig /ldconfig-runtime/ldconfig

ENTRYPOINT ["/cxi-dra-driver"]
