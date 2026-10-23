# syntax=docker/dockerfile:1
#
# Release image, built by GoReleaser (dockers_v2) from the binaries it has
# already built: the build context holds $TARGETPLATFORM/agent. Keep the
# runtime stage identical to Dockerfile.

# The debug variant of the runtime image carries busybox, used only to
# create the mount points; nothing else from it reaches the final image.
FROM gcr.io/distroless/static-debian12:debug-nonroot@sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee AS fs
USER 0
RUN ["/busybox/mkdir", "-p", "/rootfs/data", "/rootfs/downloads"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM

COPY ${TARGETPLATFORM}/agent /usr/local/bin/agent
COPY --from=fs --chown=65532:65532 /rootfs/ /

ENV NABBED_DATA_DIR=/data \
    NABBED_DOWNLOAD_DIR=/downloads \
    NABBED_CONFIG=/config/agent.yaml \
    NABBED_TORRENT_PORT=6881

VOLUME ["/data", "/downloads"]
EXPOSE 6881/tcp 6881/udp
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/agent"]
CMD ["run"]
