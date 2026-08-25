# zync-agent: opencode harness + zync client for running background agents
# on the homeserver. HOME (/root) is expected to be a persistent volume so
# opencode auth (OAuth), sessions, and the zync registry survive restarts.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/zync ./cmd/zync \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/zx ./cmd/zx

FROM debian:bookworm-slim
ARG OPENCODE_VERSION=1.18.22
RUN apt-get update \
 && apt-get install -y --no-install-recommends git curl ca-certificates unzip ripgrep \
 && rm -rf /var/lib/apt/lists/*

# opencode ships a self-contained binary; the install script drops it in
# $HOME/.opencode/bin, but /root is volume-mounted at runtime, so relocate it.
RUN curl -fsSL https://opencode.ai/install | bash -s -- --version "$OPENCODE_VERSION" --no-modify-path \
 && mv /root/.opencode/bin/opencode /usr/local/bin/opencode \
 && rm -rf /root/.opencode

COPY --from=build /out/zync /out/zx /usr/local/bin/
# Plugin + entrypoint live outside HOME; the entrypoint copies the plugin into
# the (volume-backed) HOME at boot.
COPY integrations/opencode/zync.js /opt/zync/plugins/zync.js
COPY docker/agent/entrypoint.sh /opt/zync/entrypoint.sh
RUN chmod +x /opt/zync/entrypoint.sh

ENV ZYNC_AUTO_HANDOFF=1
EXPOSE 4096
ENTRYPOINT ["/opt/zync/entrypoint.sh"]
