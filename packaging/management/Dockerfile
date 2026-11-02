# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# These are multi-architecture manifest digests from the Docker Official
# Images library. A bump is reviewed together with the Go/Node versions and
# the native runtime checksums in scripts/build-native-agent.sh.
ARG GO_IMAGE=golang:1.26.0-bookworm@sha256:2a0ba12e116687098780d3ce700f9ce3cb340783779646aafbabed748fa6677c
ARG NODE_IMAGE=node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20
ARG RUNTIME_IMAGE=debian:12.12-slim@sha256:d5d3f9c23164ea16f31852f95bd5959aad1c5e854332fe00f7b3a20fcc9f635c

FROM ${GO_IMAGE} AS keystone-build
WORKDIR /src
COPY go.work go.work.sum ./
COPY proto/ proto/
COPY wardstone/ wardstone/
COPY keystone/ keystone/
COPY cli/ cli/
COPY testing/ testing/
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN cd keystone && go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/keystone ./cmd/keystone

FROM ${NODE_IMAGE} AS web-and-native-build
WORKDIR /src
# A dated, signed Debian snapshot keeps the two build-only tools repeatable.
# The native package script still verifies Node itself with its pinned SHA-256.
RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261031T000000Z bookworm main' \
      > /etc/apt/sources.list \
    && rm -f /etc/apt/sources.list.d/* \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates=20230311+deb12u1 \
      curl=7.88.1-10+deb12u15 \
      xz-utils=5.4.1-1+deb12u1 \
    && rm -rf /var/lib/apt/lists/*

COPY console/package.json console/package-lock.json console/
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
RUN cd console && npm ci
COPY console/ console/
RUN cd console && npm run build

COPY runtimes/native-agent/package.json runtimes/native-agent/package-lock.json runtimes/native-agent/
RUN cd runtimes/native-agent && npm ci
COPY runtimes/native-agent/ runtimes/native-agent/
COPY scripts/build-native-agent.sh scripts/build-native-agent.sh
ARG TARGETARCH
ARG SOURCE_DATE_EPOCH=0
RUN cd runtimes/native-agent && npm run bundle \
    && /src/scripts/build-native-agent.sh "${TARGETARCH}" /out \
    && mkdir -p /out/native-agent \
    && tar -xzf "/out/native-agent_linux_${TARGETARCH}.tar.gz" -C /out/native-agent \
    && /out/native-agent/bin/node /out/native-agent/main.mjs --health

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="Wildstone management" \
      org.opencontainers.image.description="Keystone, Console, catalog, and the per-run native agent runtime" \
      org.opencontainers.image.source="https://github.com/Wildstone-Systems/wildstone-homelab" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"

# Keystone's Statefile implementation invokes git. CA roots are needed by git
# and outbound integrations; tzdata is already part of this pinned base and is
# required for household IANA time zones.
RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261031T000000Z bookworm main' \
      > /etc/apt/sources.list \
    && rm -f /etc/apt/sources.list.d/* \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates=20230311+deb12u1 \
      git=1:2.39.5-0+deb12u3 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 wildstone \
    && useradd --uid 65532 --gid 65532 --home-dir /home/wildstone --create-home --shell /usr/sbin/nologin wildstone \
    && install -d -o 65532 -g 65532 -m 0700 /var/lib/wildstone/keystone \
    && install -d -o 65532 -g 65532 -m 0755 /tmp

COPY --from=keystone-build /out/keystone /usr/local/bin/keystone
COPY --from=web-and-native-build /src/console/dist/ /usr/share/wildstone/console/
COPY --from=web-and-native-build /out/native-agent/ /usr/lib/wildstone/native-agent/
COPY packages/ /usr/share/wildstone/packages/
COPY wallpapers/ /usr/share/wildstone/wallpapers/

ENV HOME=/home/wildstone \
    TMPDIR=/tmp
USER 65532:65532
EXPOSE 9440 9441
VOLUME ["/var/lib/wildstone/keystone"]
ENTRYPOINT ["/usr/local/bin/keystone"]
CMD ["-listen=:9440", "-api-listen=:9441", "-data-dir=/var/lib/wildstone/keystone", "-console-dir=/usr/share/wildstone/console", "-catalog-dir=/usr/share/wildstone/packages", "-wallpaper-dir=/usr/share/wildstone/wallpapers", "-native-agent-dir=/usr/lib/wildstone/native-agent"]

