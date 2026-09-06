# The chat adapter image (ADR-W026 G4).
#
# Built from this repository, published by nobody: which registry a
# household trusts is its own decision (ADR-W010's closed-registry
# posture), so the chat packages take a digest-pinned `adapter_image`
# param and this file is how you produce one.
#
#   docker build -f cli/cmd/chat-adapter/Dockerfile -t <registry>/chat-adapter:1.0.0 .
#   docker push <registry>/chat-adapter:1.0.0
#   # then pass the digest — never the tag — as adapter_image.
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
