# avater — self-hosted Gravatar-compatible avatar proxy
# Multi-stage build (SPEC §19): compile the binary, bundle the onnxruntime
# shared library and the safety-classifier model, run as non-root.
#
# Build:
#   docker build -t avater .
#   # Apple-silicon hosts building for amd64: docker build --platform linux/amd64
#
# Run:
#   docker run -p 8080:8080 -p 127.0.0.1:8081:8081 \
#     -e AVATER_ADMIN_TOKEN=change-me -v avater-data:/data avater

ARG ORT_VERSION=1.29.0

# ---------------------------------------------------------------- build stage
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY internal internal
# CGO is required by the onnxruntime wrapper (dlopen at runtime, no link-time
# dependency — the library is only needed in the runtime image).
RUN CGO_ENABLED=1 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/avater ./cmd/avater

# ----------------------------------------------------------- onnxruntime stage
FROM debian:bookworm-slim AS ort
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
    && cp "/tmp/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so" /out/ \
    && cp "/tmp/onnxruntime-linux-${ORT_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so.${ORT_VERSION}" /out/ 2>/dev/null || true \
    && rm -rf /tmp/ort.tgz /tmp/onnxruntime-linux-*

# -------------------------------------------------------------- model download
FROM debian:bookworm-slim AS model
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
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --shell /usr/sbin/nologin avater \
    && mkdir -p /data /models && chown -R avater:avater /data /models

COPY --from=build /out/avater /usr/local/bin/avater
COPY --from=ort /out/libonnxruntime.so* /usr/local/lib/
COPY --from=model /out/models/ /models/
RUN ldconfig

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
