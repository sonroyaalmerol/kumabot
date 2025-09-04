# --- Stage 1: build FFmpeg n7.0 with libopus (shared) ---
FROM debian:trixie-slim AS ffmpeg

ARG FFMPEG_VERSION=n7.0
ARG MAKEFLAGS=-j$(nproc)

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    pkg-config \
    git \
    ca-certificates \
    curl \
    yasm \
    nasm \
    libopus-dev \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /tmp

# Fetch FFmpeg source
RUN git clone --depth 1 --branch ${FFMPEG_VERSION} https://github.com/FFmpeg/FFmpeg.git ffmpeg

WORKDIR /tmp/ffmpeg

# Configure FFmpeg shared build with libopus enabled
RUN ./configure \
    --prefix=/opt/ffmpeg/${FFMPEG_VERSION} \
    --pkg-config-flags="--static" \
    --enable-gpl \
    --enable-version3 \
    --enable-shared \
    --disable-static \
    --disable-debug \
    --disable-doc \
    --disable-programs \ 
    --enable-libopus

RUN make ${MAKEFLAGS} && make install

# --- Stage 2: build Go app with CGO against FFmpeg n7.0 ---
# Use the latest stable Go 1.x. There's no 1.25 yet; use 1.25+.
FROM golang:1.25-trixie AS builder

WORKDIR /app

# Install build deps (pkg-config) and copy FFmpeg from previous stage
RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config \
  && rm -rf /var/lib/apt/lists/*

# Copy FFmpeg install (libs+headers+pkgconfig) into /opt/ffmpeg/n7.0
COPY --from=ffmpeg /opt/ffmpeg/n7.0 /opt/ffmpeg/n7.0

# Go module download/cache
COPY go.mod go.sum ./
RUN go mod download

# App source
COPY . .

# Set CGO env so go-astiav finds FFmpeg headers/libs
ENV CGO_ENABLED=1
ENV CGO_CFLAGS="-I/opt/ffmpeg/n7.0/include"
ENV CGO_LDFLAGS="-L/opt/ffmpeg/n7.0/lib"
ENV PKG_CONFIG_PATH="/opt/ffmpeg/n7.0/lib/pkgconfig"

# Optional: reduce binary size
RUN go build -ldflags="-s -w" -o /out/kumabot ./cmd/kumabot

# --- Stage 3: slim runtime with shared libs only ---
FROM debian:trixie-slim

# Minimal runtime deps and timezone data
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    libopus0 \
  && rm -rf /var/lib/apt/lists/*

# Copy FFmpeg shared libs (no CLI) from the ffmpeg stage
# These are needed by go-astiav at runtime.
COPY --from=ffmpeg /opt/ffmpeg/n7.0/lib /opt/ffmpeg/n7.0/lib
COPY --from=ffmpeg /opt/ffmpeg/n7.0/share /opt/ffmpeg/n7.0/share

# Ensure dynamic linker can find FFmpeg libs
ENV LD_LIBRARY_PATH="/opt/ffmpeg/n7.0/lib:${LD_LIBRARY_PATH}"

# Non-root user
RUN useradd -m -u 10001 kumabot
USER kumabot

WORKDIR /app
ENV DATA_DIR=/app/data
RUN mkdir -p /app/data /app/data/cache /app/data/cache/tmp

# Copy app
COPY --from=builder /out/kumabot /usr/local/bin/kumabot

# App env
ENV REGISTER_COMMANDS_ON_BOT=false

ENTRYPOINT ["/usr/local/bin/kumabot"]