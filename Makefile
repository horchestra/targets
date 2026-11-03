# targets: the programs pack starts (pack-target-docker, -ansible, -k8s).  `make help` lists the targets.
#
# Each target is a Go module of its own (docker/, ansible/, k8s/) with its own go.mod and main.go.

GO      ?= go
BIN     ?= bin
TARGETS ?= docker ansible k8s

.DEFAULT_GOAL := help
.PHONY: help oci-proxy build dist images test vet fmt tidy install clean $(TARGETS)

help: ## list the targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'

oci-proxy: ## the oci-proxy programs the ansible target carries to the hosts (linux/amd64, linux/arm64)
	@for a in amd64 arm64; do \
		(cd ansible && CGO_ENABLED=0 GOOS=linux GOARCH=$$a $(GO) build -trimpath -ldflags='-s -w' -o internal/ansible/ociproxy/oci-proxy-linux-$$a ./cmd/oci-proxy) || exit 1; \
	done

build: oci-proxy ## build pack-target-docker, -ansible and -k8s into bin/
	@for t in $(TARGETS); do \
		echo "build pack-target-$$t"; \
		(cd $$t && $(GO) build -o ../$(BIN)/pack-target-$$t .) || exit 1; \
	done

PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

dist: oci-proxy ## per platform archives for a release: dist/pack-target-<name>_<os>_<arch>.tar.gz (what release.yaml's targets: points at)
	@for t in $(TARGETS); do for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; tmp=$$(mktemp -d); \
		echo "dist pack-target-$$t $$os/$$arch"; \
		(cd $$t && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -o $$tmp/pack-target-$$t .) || exit 1; \
		mkdir -p dist; tar -C $$tmp -czf dist/pack-target-$${t}_$${os}_$${arch}.tar.gz pack-target-$$t; rm -rf $$tmp; \
	done; done

REGISTRY ?= ghcr.io/horchestra
VERSION  ?= dev
IMAGE_PLATFORMS ?= linux/amd64,linux/arm64

images: ## the images pack runs a target from (runtime: docker): $(REGISTRY)/targets/<name>:$(VERSION), for this machine's arch; PUSH=1 builds them for IMAGE_PLATFORMS and pushes them
	@for t in $(TARGETS); do \
		echo "image $(REGISTRY)/targets/$$t:$(VERSION)"; \
		if [ -n "$(PUSH)" ]; then \
			docker buildx build --platform $(IMAGE_PLATFORMS) --push -t $(REGISTRY)/targets/$$t:$(VERSION) $$t || exit 1; \
		else \
			docker build -t $(REGISTRY)/targets/$$t:$(VERSION) $$t || exit 1; \
		fi; \
	done

$(TARGETS): oci-proxy ## build one of them: make docker
	cd $@ && $(GO) build -o ../$(BIN)/pack-target-$@ .

test: ## go test -race of every module
	@for t in $(TARGETS); do \
		echo "test $$t"; \
		(cd $$t && $(GO) test -race ./...) || exit 1; \
	done

vet: ## go vet of every module
	@for t in $(TARGETS); do (cd $$t && $(GO) vet ./...) || exit 1; done

fmt: ## gofmt of every module
	gofmt -l -w $(TARGETS)

tidy: ## go mod tidy of every module
	@for t in $(TARGETS); do (cd $$t && $(GO) mod tidy) || exit 1; done

install: build ## copy the programs to $$PREFIX (default ~/bin), for PACK_TARGET_PATH
	mkdir -p $(or $(PREFIX),$(HOME)/bin)
	cp $(BIN)/pack-target-* $(or $(PREFIX),$(HOME)/bin)/

clean: ## remove bin/ and dist/
	rm -rf $(BIN) dist
