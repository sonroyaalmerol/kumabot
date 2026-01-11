# --- Stage 1: build Go app against Debian Trixie FFmpeg (7.1) ---
FROM golang:1.25-trixie AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
  pkg-config \
  libavdevice-dev \
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


# --- Stage 2: runtime with Trixie FFmpeg libs (no manual LD paths) ---
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  tzdata \
  libavcodec61 \
  libavformat61 \
  libavutil59 \
  libswresample5 \
  libswscale8 \
  libavfilter10 \
  libavdevice61 \
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
