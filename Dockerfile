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

FROM alpine:3.24.1

RUN addgroup -S -g 10001 homeagent \
    && adduser -S -D -H -u 10001 -G homeagent homeagent \
    && mkdir -p /var/lib/home-agent \
    && chown 10001:10001 /var/lib/home-agent

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/home-agent /usr/local/bin/home-agent

USER 10001:10001

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/home-agent"]
