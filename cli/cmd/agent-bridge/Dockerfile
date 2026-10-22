# A standalone image of the agent bridge, for running it outside
# Wildstone or testing it on its own.
#
# Wildstone itself does not use this. The release ships the bridge
# binary, the installer puts it on every node at /srv/wildstone/bin, and
# the agent-hermes package runs it in Google's public distroless image
# with that directory mounted read-only (the same shape ADR-W033 B7.1
# chose for the chat adapter, cli/cmd/chat-adapter/Dockerfile), so no
# image of ours is published and a node needs no registry credential.
#
#   docker build -f cli/cmd/agent-bridge/Dockerfile -t agent-bridge .
#
# Static, scratch-based, non-root: the bridge opens outbound HTTPS
# connections to Keystone and Hermes and listens on one internal-network
# port for Hermes's own MCP calls. No shell, no package manager, and
# nothing to read if somebody does get into it — its two credentials
# arrive as environment variables from Wildstone's own secret store.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY keystone/ keystone/
COPY proto/ proto/
COPY wardstone/ wardstone/
COPY cli/ cli/
COPY testing/ testing/
ENV CGO_ENABLED=0
RUN cd cli && go build -trimpath -ldflags "-s -w" -o /out/agent-bridge ./cmd/agent-bridge

FROM scratch
# Keystone and Hermes are both reached over HTTPS in the general case, so
# the image needs root certificates and nothing else.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/agent-bridge /agent-bridge
USER 65534:65534
ENTRYPOINT ["/agent-bridge"]
