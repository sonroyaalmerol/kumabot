# --- Stage 1: build Go app against Debian Sid FFmpeg (8.0) ---
FROM golang:1.26-trixie AS builder

WORKDIR /app

# Add Debian Sid for FFmpeg 8.0 (required by go-astiav v0.40.0+)
RUN echo 'deb http://deb.debian.org/debian sid main' > /etc/apt/sources.list.d/sid.list && \
  apt-get update && apt-get install -y --no-install-recommends \
  -t sid \
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


# --- Stage 2: runtime with Sid FFmpeg 8.0 libs ---
FROM debian:sid-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  tzdata \
  libavcodec62 \
  libavformat62 \
  libavutil60 \
  libswresample6 \
  libswscale9 \
  libavfilter11 \
  libavdevice62 \
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
