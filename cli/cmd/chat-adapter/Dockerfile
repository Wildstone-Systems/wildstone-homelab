# A standalone image of the chat adapter, for running it outside
# Wildstone or testing it on its own.
#
# Wildstone itself does not use this. The release ships the adapter
# binary, the installer puts it on every node at /srv/wildstone/bin,
# and the chat packages run it in a public distroless image with that
# directory mounted read-only (ADR-W033 B7.1), so no image is published
# and a node needs no registry credential.
#
#   docker build -f cli/cmd/chat-adapter/Dockerfile -t chat-adapter .
#
# Static, scratch-based, non-root: the adapter opens two outbound HTTPS
# connections and needs nothing else. No shell, no package manager, and
# nothing to read if somebody does get into it — the credentials arrive
# as environment variables from Wildstone's own secret store.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY keystone/ keystone/
COPY proto/ proto/
COPY wardstone/ wardstone/
COPY cli/ cli/
COPY testing/ testing/
ENV CGO_ENABLED=0
RUN cd cli && go build -trimpath -ldflags "-s -w" -o /out/chat-adapter ./cmd/chat-adapter

FROM scratch
# Telegram is an HTTPS API, so the image needs root certificates and
# nothing else.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/chat-adapter /chat-adapter
USER 65534:65534
ENTRYPOINT ["/chat-adapter"]
