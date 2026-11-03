# The image of the docker target: pack-target-docker and the docker CLI it drives the engine with. pack runs it with
# runtime: {type: docker} (github.com/horchestra/pack, docs/targets.md); the engine is reached through a mount of its
# socket (mounts: [/var/run/docker.sock:/var/run/docker.sock]) or over ssh, as from a machine.
#
#   docker build -t registry.example/targets/docker:0.2.0 docker
ARG GO=1.27
ARG DOCKER_CLI=29-cli

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
# the go.mod of the sources again: the same replace
RUN go mod edit -dropreplace github.com/horchestra/pack \
 && if [ -f /pack/go.mod ]; then go mod edit -replace github.com/horchestra/pack=/pack; fi
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/pack-target-docker .

FROM docker:${DOCKER_CLI}
LABEL io.horchestra.pack.target=docker \
      io.horchestra.pack.protocol=pack.target.v1 \
      org.opencontainers.image.source=https://github.com/horchestra/targets
# root: the engine's socket is root's; HOME is where pack mounts the release's directory for the program
ENV HOME=/home/pack
RUN mkdir -p /home/pack
COPY --from=build /out/pack-target-docker /usr/local/bin/pack-target-docker
EXPOSE 7353
ENTRYPOINT ["/usr/local/bin/pack-target-docker"]
