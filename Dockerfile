FROM golang:1.25-trixie AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1
RUN go build -ldflags "-s -w" -o /out/kumabot ./cmd/kumabot

FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      tzdata \
      ffmpeg \
      libopus-dev \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 10001 kumabot
USER kumabot

WORKDIR /app
ENV DATA_DIR=/app/data
RUN mkdir -p /app/data /app/data/cache /app/data/cache/tmp

COPY --from=builder /out/kumabot /usr/local/bin/kumabot

ENV REGISTER_COMMANDS_ON_BOT=false

ENTRYPOINT ["/usr/local/bin/kumabot"]
