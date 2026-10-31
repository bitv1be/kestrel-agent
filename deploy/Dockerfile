# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.27.1
ARG ALPINE_VERSION=3.23

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/agent ./cmd/agent

FROM alpine:${ALPINE_VERSION} AS runtime
ARG FULL=1
ARG ALLOW_DOAS=0
RUN apk add --no-cache \
      bash coreutils findutils grep sed gawk diffutils patch util-linux less file tree procps-ng htop tmux \
      curl wget openssh-client-default rsync bind-tools iputils iproute2 netcat-openbsd openssl ca-certificates tzdata \
      tar gzip bzip2 xz zip unzip git jq yq-go ripgrep fd sqlite make nano vim \
      python3 py3-pip nodejs npm tini doas shadow \
    && if [ "$FULL" = 1 ]; then apk add --no-cache build-base; fi \
    && addgroup -S agent \
    && adduser -S -D -h /workspace -s /bin/bash -G agent agent \
    && if [ "$ALLOW_DOAS" = 1 ]; then printf 'permit nopass agent as root\n' > /etc/doas.d/agent.conf; fi \
    && mkdir -p /data /workspace \
    && chown agent:agent /data /workspace
COPY --from=builder /out/agent /usr/local/bin/agent
ENV AGENT_DATA_DIR=/data AGENT_WORKSPACE=/workspace AGENT_LOG_FORMAT=json SHELL=/bin/bash
USER agent
WORKDIR /workspace
VOLUME /data /workspace
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/agent"]
CMD ["gateway"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8081/healthz || exit 1
