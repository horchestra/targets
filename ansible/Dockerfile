# The image of the ansible target: pack-target-ansible with ansible-core and the ssh client it reaches the hosts
# with, so that the machine pack runs on needs neither. pack runs it with runtime: {type: docker}
# (github.com/horchestra/pack, docs/targets.md): the keys and known_hosts are mounts, the agent ssh_agent: true.
#
#   docker build -t registry.example/targets/ansible:0.2.0 ansible
ARG GO=1.27
ARG PYTHON=3.13

FROM golang:${GO} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# the oci-proxy programs the target carries to the hosts, embedded in it
RUN for a in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags='-s -w' -o internal/ansible/ociproxy/oci-proxy-linux-$a ./cmd/oci-proxy || exit 1; \
    done
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/pack-target-ansible .

FROM python:${PYTHON}-slim
ARG ANSIBLE_CORE=2.19.*
LABEL io.horchestra.pack.target=ansible \
      io.horchestra.pack.protocol=pack.target.v1 \
      org.opencontainers.image.source=https://github.com/horchestra/targets
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-client sshpass \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "ansible-core==${ANSIBLE_CORE}" \
 && useradd --create-home --home-dir /home/pack --uid 1000 pack
COPY --from=build /out/pack-target-ansible /usr/local/bin/pack-target-ansible
USER pack
# the home is the release's directory, a mount: ssh's control sockets cannot be made on every one (Docker Desktop's),
# and need not outlive the container
ENV HOME=/home/pack \
    ANSIBLE_SSH_CONTROL_PATH_DIR=/tmp/ansible-cp
EXPOSE 7353
ENTRYPOINT ["/usr/local/bin/pack-target-ansible"]
