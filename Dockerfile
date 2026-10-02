FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24 AS build

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

FROM --platform=$TARGETPLATFORM node:24.19.0-alpine3.24

RUN apk add --no-cache ca-certificates wget \
    && addgroup -S -g 10001 homeagent \
    && adduser -S -D -H -u 10001 -G homeagent homeagent \
    && mkdir -p /var/lib/home-agent \
    && mkdir -p /etc/home-agent \
    && chown 10001:10001 /var/lib/home-agent

RUN npm install --global --omit=dev @earendil-works/pi-coding-agent@1.0.0 \
    && test "$(pi --version)" = "1.0.0" \
    && npm cache clean --force

COPY third_party/pi-web-search /usr/local/lib/home-agent/pi-web-search

COPY --from=build /out/home-agent /usr/local/bin/home-agent

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/home-agent"]
