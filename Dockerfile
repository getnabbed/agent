# Builds the agent image from source: `docker build -t nabbed-agent .`
# Releases use release.Dockerfile, which packages the binaries GoReleaser
# already built and signed, so both produce the same runtime image.
#
# Base images are pinned by digest in the FROM lines, where Dependabot can
# find and update them.

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""

# GOTOOLCHAIN=local: build with exactly the pinned image's Go, never a
# toolchain downloaded at build time.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build \
      -ldflags "-s -w \
        -X github.com/getnabbed/agent/internal/version.Version=${VERSION} \
        -X github.com/getnabbed/agent/internal/version.Commit=${COMMIT} \
        -X github.com/getnabbed/agent/internal/version.Date=${DATE}" \
      -o /out/agent ./cmd/agent \
 && mkdir -p /out/rootfs/data /out/rootfs/downloads

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev

LABEL org.opencontainers.image.title="nabbed-agent" \
      org.opencontainers.image.description="Nabbed download agent" \
      org.opencontainers.image.source="https://github.com/getnabbed/agent" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/agent /usr/local/bin/agent
# Mount points owned by the nonroot user (65532), so a new named volume
# inherits ownership. Bind mounts must be owned by 65532 or run with --user.
COPY --from=build --chown=65532:65532 /out/rootfs/ /

ENV NABBED_DATA_DIR=/data \
    NABBED_DOWNLOAD_DIR=/downloads \
    NABBED_CONFIG=/config/agent.yaml \
    NABBED_TORRENT_PORT=6881

VOLUME ["/data", "/downloads"]
EXPOSE 6881/tcp 6881/udp
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/agent"]
CMD ["run"]
