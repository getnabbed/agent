# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/getnabbed/agent/internal/version.Version=${VERSION}" \
      -o /out/agent ./cmd/agent \
 && mkdir -p /out/data

# distroless/static: no shell, no package manager, CA roots included.
# The nonroot user is uid/gid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent /usr/local/bin/agent
# Pre-create /data owned by nonroot so a new named volume inherits that
# ownership. Bind mounts must be chowned to 65532 on the host instead.
COPY --from=build --chown=65532:65532 /out/data /data

# Credentials and state. Mount a named volume here; it must persist across
# container recreation or the agent has to be paired again.
ENV NABBED_AGENT_DATA_DIR=/data
VOLUME ["/data"]

# No TTY in a container: JSON logs by default. Override with --log-format text.
ENTRYPOINT ["/usr/local/bin/agent", "--log-format", "json"]
CMD ["run"]
