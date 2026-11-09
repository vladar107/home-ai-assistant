FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/home-agent \
    ./cmd/home-agent

FROM --platform=$TARGETPLATFORM node:24.22.2-alpine3.24@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8

RUN apk add --no-cache ca-certificates wget \
    && addgroup -S -g 10001 homeagent \
    && adduser -S -D -H -u 10001 -G homeagent homeagent \
    && mkdir -p /var/lib/home-agent \
    && mkdir -p /etc/home-agent \
    && chown 10001:10001 /var/lib/home-agent

COPY third_party/pi-runtime /usr/local/lib/home-agent/pi-runtime

RUN cd /usr/local/lib/home-agent/pi-runtime \
    && npm ci --omit=dev --ignore-scripts \
    && ln -s /usr/local/lib/home-agent/pi-runtime/node_modules/.bin/pi /usr/local/bin/pi \
    && test "$(pi --version)" = "1.0.0" \
    && npm cache clean --force

COPY third_party/pi-web-search /usr/local/lib/home-agent/pi-web-search

COPY --from=build /out/home-agent /usr/local/bin/home-agent

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/home-agent"]
