# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates tzdata
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/afdd ./cmd/server     && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/afdd-sim ./cmd/simulator

FROM alpine:3.22
LABEL org.opencontainers.image.title="Digital Twin AFDD API and Simulator"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S afdd && adduser -S -G afdd afdd \
    && mkdir -p /data && chown afdd:afdd /data
COPY --from=build /out/afdd /out/afdd-sim /usr/local/bin/
USER afdd
WORKDIR /data
ENV DATA_DIR=/data
# HTTP API, MQTT, MQTT/TLS, MQTT over WebSocket
EXPOSE 4500 1883 8883 9002
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD ["afdd", "healthcheck"]
ENTRYPOINT ["afdd"]
CMD ["serve"]
