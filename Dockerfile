# avater — self-hosted Gravatar-compatible avatar proxy
# Multi-stage build (SPEC §19): cross-compile the binary, bundle the
# onnxruntime shared library and the safety-classifier model, run as non-root.
#
# Multi-arch, near-QEMU-free:
#   - the Go build stage runs on the builder's native platform with a per-target
#     cross C compiler (only the tiny onnxruntime cgo shim needs cgo),
#   - the library/model stages only download & extract arch-specific files,
#   - QEMU only executes the runtime stage's single apt/user setup layer.
#
# Build (any host):
#   docker build -t avater --platform linux/amd64,linux/arm64 .
#   # single arch: docker build -t avater .

ARG ORT_VERSION=1.29.0

# ---------------------------------------------------------------- build stage
# Pinned to the builder's native platform: cross-compiles to TARGETARCH
# (never runs under QEMU).
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
WORKDIR /src

# Cross C toolchain for the cgo shim of onnxruntime_go.
RUN apt-get update && apt-get install -y --no-install-recommends \
      $(case "$TARGETARCH" in \
          amd64) echo "crossbuild-essential-amd64" ;; \
          arm64) echo "crossbuild-essential-arm64" ;; \
          *) echo "unsupported TARGETARCH $TARGETARCH" >&2; exit 1 ;; \
        esac) \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY internal internal
# CGO is required by the onnxruntime wrapper (the library itself is dlopen'd
# at runtime — no link-time dependency on onnxruntime).
RUN --mount=type=cache,target=/root/.cache/go-build \
    case "$TARGETARCH" in \
      amd64) CC=x86_64-linux-gnu-gcc ;; \
      arm64) CC=aarch64-linux-gnu-gcc ;; \
    esac; \
    CGO_ENABLED=1 GOOS=linux GOARCH=$TARGETARCH CC=$CC \
    go build -trimpath -ldflags "-s -w" -o /out/avater ./cmd/avater

# ----------------------------------------------------------- onnxruntime stage
# File operations only, so pin to the builder platform (no QEMU).
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS ort
ARG ORT_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# Official Microsoft release tarball; API-compatible with yalue/onnxruntime_go.
RUN case "$TARGETARCH" in \
      amd64) ORT_ARCH=x64 ;; \
      arm64) ORT_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH $TARGETARCH" && exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/ort.tgz \
       "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}.tgz" \
    && tar -xzf /tmp/ort.tgz -C /tmp \
    && mkdir -p /out \
    && cp /tmp/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so* /out/ \
    && rm -rf /tmp/ort.tgz /tmp/onnxruntime-linux-*

# -------------------------------------------------------------- model download
# File operations only, so pin to the builder platform (no QEMU).
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS model
# Override for air-gapped/mirror environments, e.g. https://hf-mirror.com
ARG HF_ENDPOINT=https://huggingface.co
# Pinned digest; the server re-verifies at startup (SPEC §9.2/§17).
ARG MODEL_SHA256=8c28c49d9075f3ad15ebdc2961f02d5b3f99be944815b848b49c9f0e6f3fb689
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /out/models \
    && curl -fsSL -o /out/models/image-safety-classifier-xs.onnx \
       "${HF_ENDPOINT}/OwenElliott/image-safety-classifier-xs/resolve/main/onnx/image-safety-classifier-xs.onnx" \
    && echo "${MODEL_SHA256}  /out/models/image-safety-classifier-xs.onnx" | sha256sum -c -

# ----------------------------------------------------------------- run stage
# No RUN steps: nothing executes under emulation, so multi-arch needs no QEMU.
FROM debian:bookworm-slim
LABEL org.opencontainers.image.title="avater" \
      org.opencontainers.image.description="Self-hosted Gravatar-compatible avatar proxy with content moderation" \
      org.opencontainers.image.source="https://github.com/liueic/avatar" \
      org.opencontainers.image.licenses="MIT"

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --shell /usr/sbin/nologin avater \
    && mkdir -p /data /models && chown -R avater:avater /data /models

COPY --from=build /out/avater /usr/local/bin/avater
COPY --from=ort /out/libonnxruntime.so* /usr/local/lib/
COPY --from=model /out/models/ /models/

ENV LD_LIBRARY_PATH=/usr/local/lib

USER avater
WORKDIR /data

# Resource budget for 2 vCPU / 2 GB (SPEC §14/§19).
ENV GOMEMLIMIT=1500MiB
ENV GOGC=50
ENV AVATER_CACHE_DIR=/data
ENV AVATER_MODERATION_MODEL_PATH=/models/image-safety-classifier-xs.onnx
ENV AVATER_MODERATION_ORT_LIB_PATH=libonnxruntime.so
ENV AVATER_MODERATION_MODEL_SHA256=8c28c49d9075f3ad15ebdc2961f02d5b3f99be944815b848b49c9f0e6f3fb689

EXPOSE 8080 8081
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD ["/usr/local/bin/avater", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/avater"]
