# The image of the k8s target: pack-target-k8s alone, client-go in it. pack runs it with runtime: {type: docker}
# (github.com/horchestra/pack, docs/targets.md); the kubeconfig is a mount (~/.kube/config:/home/pack/.kube/config:ro).
#
#   docker build -t registry.example/targets/k8s:0.2.0 k8s
ARG GO=1.27

FROM golang:${GO} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/pack-target-k8s . \
 && mkdir -p /out/home/pack

FROM gcr.io/distroless/static-debian12:nonroot
LABEL io.horchestra.pack.target=k8s \
      io.horchestra.pack.protocol=pack.target.v1 \
      org.opencontainers.image.source=https://github.com/horchestra/targets
COPY --from=build --chown=65532:65532 /out/home/pack /home/pack
COPY --from=build /out/pack-target-k8s /usr/local/bin/pack-target-k8s
ENV HOME=/home/pack
EXPOSE 7353
ENTRYPOINT ["/usr/local/bin/pack-target-k8s"]
