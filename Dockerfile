# --- Stage 1: build Go app against distro FFmpeg 7.1 (Trixie) ---
FROM golang:1.25-trixie AS builder

WORKDIR /app

# Install pkg-config and FFmpeg dev packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config \
    libavcodec-dev \
    libavformat-dev \
    libavutil-dev \
    libswresample-dev \
    libswscale-dev \
    libavfilter-dev \
    libopus-dev \
  && rm -rf /var/lib/apt/lists/*

# Go modules
COPY go.mod go.sum ./
RUN go mod download

# App source
COPY . .

# Enable CGO so go-astiav links to system FFmpeg
ENV CGO_ENABLED=1

# Build
RUN go build -ldflags "-s -w" -o /out/kumabot ./cmd/kumabot


# --- Stage 2: slim runtime with FFmpeg 7.1 shared libs from Trixie ---
FROM debian:trixie-slim

# Install runtime FFmpeg libs and minimal deps (no ffmpeg CLI needed)
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    libavcodec60 \
    libavformat60 \
    libavutil58 \
    libswresample4 \
    libswscale7 \
    libavfilter9 \
    libopus0 \
  && rm -rf /var/lib/apt/lists/*

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