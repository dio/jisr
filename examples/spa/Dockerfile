# syntax=docker/dockerfile:1
# ─────────────────────────────────────────────────────────────────────────────
# spa — Envoy dynamic module serving a Vite + React SPA
#
# Supports linux/amd64 and linux/arm64.
# Cross-compilation uses zig cc as the CGO C compiler — no native Linux
# toolchain required. Works from macOS, Linux, or any CI host.
#
# Build (multi-arch, push to registry):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t ghcr.io/dio/jisr/spa:latest --push .
#
# Build (single arch, local load):
#   docker buildx build --platform linux/amd64 --load -t spa:latest .
#
# Run:
#   docker run --rm -p 10000:10000 spa:latest
#   curl http://localhost:10000/api/hello
# ─────────────────────────────────────────────────────────────────────────────

# ── Stage 1: build the Vite SPA ──────────────────────────────────────────────
FROM node:22-alpine AS ui-builder

WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --prefer-offline
COPY ui/ ./
RUN npm run build
# output: /src/ui/dist/


# ── Stage 2: cross-compile the Go .so with zig cc ────────────────────────────
#
# The builder image always runs on the *host* architecture (BuildKit emulation
# is slow and unnecessary here). zig cc cross-compiles to the *target* arch.
#
# Why zig cc?  Go's c-shared linker passes --unresolved-symbols=ignore-all,
# which GNU ld accepts but LLVM lld (used by zig cc) rejects. A thin wrapper
# script strips that flag before forwarding to zig cc.
FROM golang:1.26-bookworm AS so-builder

ARG TARGETARCH
ARG ZIG_VERSION=0.16.0

# Install build deps: xz-utils to unpack the Zig tarball, curl to fetch it.
RUN apt-get update && apt-get install -y --no-install-recommends xz-utils curl \
    && rm -rf /var/lib/apt/lists/*

# Install Zig. The builder runs on the host arch; zig cc cross-compiles to target.
# Zig release URL format: zig-{arch}-linux-{version}.tar.xz
RUN set -eux; \
    ARCH="$(uname -m)"; \
    ZIG_URL="https://ziglang.org/download/${ZIG_VERSION}/zig-${ARCH}-linux-${ZIG_VERSION}.tar.xz"; \
    curl -fsSL "${ZIG_URL}" | tar -xJ -C /opt; \
    ln -s "/opt/zig-${ARCH}-linux-${ZIG_VERSION}/zig" /usr/local/bin/zig; \
    zig version

# Wrapper script: strips --unresolved-symbols which lld rejects.
# ZIG_TARGET is injected at build time via the RUN env below.
RUN printf '#!/usr/bin/env bash\n\
ZIG_TARGET="${ZIG_TARGET}"\n\
args=()\n\
for arg in "$@"; do\n\
  case "$arg" in\n\
    --unresolved-symbols=*) ;;\n\
    -Wl,--unresolved-symbols=*) ;;\n\
    *) args+=("$arg") ;;\n\
  esac\n\
done\n\
exec zig cc -target "${ZIG_TARGET}" "${args[@]}"\n' > /usr/local/bin/zigcc \
    && chmod +x /usr/local/bin/zigcc

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# Copy Vite dist from stage 1 into the expected embed path.
COPY --from=ui-builder /src/ui/dist/ ui/dist/
COPY . .

# Derive zig cross-compile target from Docker's TARGETARCH.
#   linux/amd64  -> x86_64-linux-gnu
#   linux/arm64  -> aarch64-linux-gnu
# Always -gnu (glibc). Envoy links glibc; musl .so = runtime crash at dlopen.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ZIG_TARGET=x86_64-linux-gnu  GOOS=linux GOARCH=amd64 ;; \
      arm64) ZIG_TARGET=aarch64-linux-gnu GOOS=linux GOARCH=arm64 ;; \
      *)     echo "unsupported TARGETARCH=${TARGETARCH}"; exit 1   ;; \
    esac; \
    export ZIG_TARGET GOOS GOARCH; \
    mkdir -p /out; \
    CC=zigcc CGO_ENABLED=1 \
    go build \
      -trimpath \
      -buildmode=c-shared \
      -o /out/libspa.so \
      ./cmd


# ── Stage 3: distroless Envoy ─────────────────────────────────────────────────
# Multi-arch manifest: envoyproxy/envoy:distroless-v1.37.1 ships amd64 + arm64.
# Distroless has no shell; GODEBUG must be set via ENV not a wrapper script.
FROM envoyproxy/envoy:distroless-v1.37.1

# Dynamic module: Envoy dlopen("libspa.so") from ENVOY_DYNAMIC_MODULES_SEARCH_PATH.
COPY --from=so-builder /out/libspa.so /etc/envoy/libspa.so

COPY envoy.yaml /etc/envoy/envoy.yaml

# Required at runtime for Go-based dynamic modules.
# Disables CGO pointer checks that fire on valid cross-boundary pointers.
ENV GODEBUG=cgocheck=0
ENV ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy

EXPOSE 10000 9901

# Entrypoint is already /usr/local/bin/envoy; we just override the default config path.
CMD ["-c", "/etc/envoy/envoy.yaml", "--log-level", "info"]
