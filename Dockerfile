FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS builder

ARG TARGETARCH
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
  pkg-config libavdevice-dev libavcodec-dev libavformat-dev \
  libavutil-dev libswresample-dev libswscale-dev libavfilter-dev libopus-dev \
  && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the binary inside Docker to handle CGO correctly for the TARGETARCH
RUN CGO_ENABLED=1 GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /kumabot ./cmd/kumabot

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates tzdata libavcodec61 libavformat61 libavutil59 \
  libswresample5 libswscale8 libavfilter10 libavdevice61 libopus0 \
  && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 10001 kumabot
USER kumabot
WORKDIR /app
RUN mkdir -p /app/data /app/data/cache /app/data/cache/tmp

COPY --from=builder /kumabot /usr/local/bin/kumabot

ENV DATA_DIR=/app/data
ENV REGISTER_COMMANDS_ON_BOT=false

ENTRYPOINT ["/usr/local/bin/kumabot"]
