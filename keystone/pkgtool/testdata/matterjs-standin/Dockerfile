# A STAND-IN for ghcr.io/matter-js/matterjs-server, used by
# TestMatterServerServesHomeAssistant only when the pinned image cannot
# be pulled (a sandbox whose egress refuses GHCR's blob host) and
# WILDSTONE_MATTER_STANDIN=1 asks for it.
#
# It is upstream's own docker/matterjs-server/Dockerfile (matterjs-server
# v1.4.0) with the apt steps left out: the same node:24-trixie-slim base,
# the same `npm install matter-server@<version>` into /app, the same
# /data owned by 1000:1000, the same env defaults, USER, ENTRYPOINT and
# healthcheck script. What is missing is BLE's native build (bluez,
# node-gyp toolchain) and ping's CAP_NET_RAW, neither of which the test
# exercises. It is NOT the pinned image: a pass on it proves the
# package's configuration against the same server code, not the digest.
FROM node:24-trixie-slim

WORKDIR /app

ARG MATTERJS_SERVER_VERSION

# extra_ca: the host's NODE_EXTRA_CA_CERTS, when the test finds one (a
# TLS-intercepting egress proxy); absent otherwise, and npm then only
# warns that the named file is missing.
RUN --mount=type=secret,id=extra_ca,required=false \
    NODE_EXTRA_CA_CERTS=/run/secrets/extra_ca \
    npm install --foreground-scripts "matter-server@${MATTERJS_SERVER_VERSION}" \
    && npm cache clean --force \
    && rm -rf /tmp/* /root/.npm \
    && mkdir /data \
    && chown 1000:1000 /data

ENV LOG_LEVEL=info
ENV STORAGE_PATH=/data

VOLUME ["/data"]

EXPOSE 5580

COPY --chmod=0755 healthcheck.sh /usr/local/bin/healthcheck.sh

HEALTHCHECK --interval=60s --timeout=10s --start-period=900s --start-interval=5s --retries=6 \
    CMD ["/usr/local/bin/healthcheck.sh"]

USER 1000:1000
ENTRYPOINT ["node", "--enable-source-maps", "/app/node_modules/matter-server/dist/esm/MatterServer.js"]
CMD []
