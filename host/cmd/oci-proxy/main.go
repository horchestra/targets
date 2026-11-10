// Command oci-proxy serves the files of OCI artifacts over plain HTTP as ordinary trees, so that apt and dnf take a
// package repository kept in a registry: GET /<repo>:<tag>/<file>. It is oci-packer's proxy with paths a package
// manager can use — the same proxy the host agent runs during a configure, as a standalone program (the e2e tests
// drive it by hand). It is configured by the environment alone:
//
//	OCI_PROXY_REGISTRY    the registry: registry.example or oci://registry.example (required)
//	OCI_PROXY_ADDR        the address to listen on (127.0.0.1:8081)
//	OCI_PROXY_PLAIN_HTTP  true: the registry speaks http, not https
//	OCI_PROXY_USER, OCI_PROXY_PASSWORD   the registry's credentials
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arenadata/oci-packer/pkg/http/proxy"

	"github.com/horchestra/targets/host/internal/agent"
)

// shutdownTimeout is how long the requests under way have to finish once it is told to stop.
const shutdownTimeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	registry := os.Getenv("OCI_PROXY_REGISTRY")
	if len(registry) == 0 {
		return errors.New("OCI_PROXY_REGISTRY is not set")
	}

	addr := os.Getenv("OCI_PROXY_ADDR")
	if len(addr) == 0 {
		addr = "127.0.0.1:8081"
	}

	opts := []proxy.Option{proxy.WithReferenceConverter(agent.Convert)}
	if v := os.Getenv("OCI_PROXY_PLAIN_HTTP"); len(v) > 0 {
		plain, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("OCI_PROXY_PLAIN_HTTP: " + err.Error())
		}
		if plain {
			opts = append(opts, proxy.WithRemotePlainHttp())
		}
	}

	if user := os.Getenv("OCI_PROXY_USER"); len(user) > 0 {
		opts = append(opts, proxy.WithCreds(user, os.Getenv("OCI_PROXY_PASSWORD")))
	}

	h, err := proxy.New("oci://"+strings.TrimPrefix(registry, "oci://"), opts...)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 30 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(sctx)
}
