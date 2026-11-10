# The image of the host target: pack-target-host alone, the agents it carries to the hosts built in. pack runs it
# with runtime: {type: docker} (github.com/horchestra/pack, docs/targets.md); the SSH key is a mount
# (~/.ssh/id:/home/pack/.ssh/id:ro) or the agent's socket.
#
#   docker build -t registry.example/targets/host:0.1.0 host
ARG GO=1.27

# pack, when the build is given its checkout (--build-context pack=<dir>): a go.mod that replaces pack with ../..
# builds against it; without, the replace goes and the module's own require is built
FROM scratch AS pack

FROM golang:${GO} AS build
COPY --from=pack / /pack/
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod edit -dropreplace github.com/horchestra/pack \
 && if [ -f /pack/go.mod ]; then go mod edit -replace github.com/horchestra/pack=/pack; fi \
 && go mod download
COPY . .
RUN go mod edit -dropreplace github.com/horchestra/pack \
 && if [ -f /pack/go.mod ]; then go mod edit -replace github.com/horchestra/pack=/pack; fi
# the agents the driver carries to the hosts, then the driver itself
RUN for a in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags='-s -w' \
        -o internal/host/agentbin/pack-host-agent-linux-$a ./cmd/agent || exit 1; \
    done
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/pack-target-host . \
 && mkdir -p /out/home/pack

# the docker CLI the agent shells out to for the container semantics (the socket is a mount)
FROM docker:29-cli AS cli

# Not distroless: the target's Discover runs nproc/free/df/ss through the agent, and the agent runs the docker CLI, so
# the image carries their tools (pack's e2e reads the facts of a container of it).
FROM debian:13-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends coreutils procps iproute2 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --home-dir /home/pack --create-home pack
LABEL io.horchestra.pack.target=host \
      io.horchestra.pack.protocol=pack.target.v1 \
      org.opencontainers.image.source=https://github.com/horchestra/targets
COPY --from=cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=build --chown=65532:65532 /out/home/pack /home/pack
COPY --from=build /out/pack-target-host /usr/local/bin/pack-target-host
USER 65532
ENV HOME=/home/pack
EXPOSE 7353
ENTRYPOINT ["/usr/local/bin/pack-target-host"]
