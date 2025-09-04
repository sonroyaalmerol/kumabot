# --- Stage 1: build FFmpeg n7.0 with libopus (shared libs, no CLI) ---
FROM debian:trixie-slim AS ffmpeg

ARG FFMPEG_VERSION=n7.0

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

# Fetch FFmpeg source at the exact tag/branch compatible with go-astiav
RUN git clone --depth 1 --branch "${FFMPEG_VERSION}" https://github.com/FFmpeg/FFmpeg.git ffmpeg

WORKDIR /tmp/ffmpeg

# Configure FFmpeg: shared libs, libopus enabled, no CLI programs to keep image slim
RUN ./configure \
    --prefix=/opt/ffmpeg/${FFMPEG_VERSION} \
    --enable-gpl \
    --enable-version3 \
    --enable-shared \
    --disable-static \
    --disable-debug \
    --disable-doc \
    --disable-programs \
    --enable-libopus

# Build and install using all available cores
RUN make -j"$(nproc)" && make install


# --- Stage 2: build Go app with CGO against FFmpeg n7.0 ---
# Using Go 1.25 on Debian trixie
FROM golang:1.25-trixie AS builder

WORKDIR /app

# Build deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config \
  && rm -rf /var/lib/apt/lists/*

# Copy FFmpeg install (headers, libs, and pkgconfig files)
COPY --from=ffmpeg /opt/ffmpeg/n7.0 /opt/ffmpeg/n7.0

# Go modules
COPY go.mod go.sum ./
RUN go mod download

# App source
COPY . .

# CGO env so go-astiav can find FFmpeg
ENV CGO_ENABLED=1
ENV CGO_CFLAGS="-I/opt/ffmpeg/n7.0/include"
ENV CGO_LDFLAGS="-L/opt/ffmpeg/n7.0/lib"
ENV PKG_CONFIG_PATH="/opt/ffmpeg/n7.0/lib/pkgconfig"

# Build the binary
RUN go build -ldflags "-s -w" -o /out/kumabot ./cmd/kumabot


# --- Stage 3: slim runtime image with only needed shared libs ---
FROM debian:trixie-slim

# Minimal runtime deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    libopus0 \
  && rm -rf /var/lib/apt/lists/*

# Copy FFmpeg shared libs (no CLI) from builder stage
COPY --from=ffmpeg /opt/ffmpeg/n7.0/lib /opt/ffmpeg/n7.0/lib
COPY --from=ffmpeg /opt/ffmpeg/n7.0/share /opt/ffmpeg/n7.0/share

# Make dynamic linker see the FFmpeg libs
ENV LD_LIBRARY_PATH="/opt/ffmpeg/n7.0/lib"

# Non-root user
RUN useradd -m -u 10001 kumabot
USER kumabot

WORKDIR /app
ENV DATA_DIR=/app/data
RUN mkdir -p /app/data /app/data/cache /app/data/cache/tmp

# Copy app binary
COPY --from=builder /out/kumabot /usr/local/bin/kumabot

# App envs
ENV REGISTER_COMMANDS_ON_BOT=false

ENTRYPOINT ["/usr/local/bin/kumabot"]