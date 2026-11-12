package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/arenadata/oci-packer/pkg/http/proxy"
	"github.com/arenadata/oci-packer/pkg/registry/reference"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// artifactProxy is the HTTP server of the artifacts' files on the loopback, for the package managers of the host.
type artifactProxy struct {
	srv *http.Server
	ln  net.Listener
}

// startProxy runs the proxy of the request's registry on 127.0.0.1:port.
func startProxy(req *agentpb.ProxyStartRequest) (*artifactProxy, error) {
	if len(req.Registry) == 0 {
		return nil, fmt.Errorf("no registry")
	}
	opts := []proxy.Option{proxy.WithReferenceConverter(Convert)}
	if req.PlainHttp {
		opts = append(opts, proxy.WithRemotePlainHttp())
	}
	if len(req.Username) > 0 {
		opts = append(opts, proxy.WithCreds(req.Username, req.Password))
	}
	h, err := proxy.New("oci://"+strings.TrimPrefix(req.Registry, "oci://"), opts...)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port))
	if err != nil {
		return nil, err
	}
	p := &artifactProxy{srv: &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}, ln: ln}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *artifactProxy) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx)
}

// Convert reads /<repo>:<tag>/<file> and /<repo>@<digest>/<file>: "@" or the first colon ends the repository,
// the first slash after it the tag or digest, the rest is the file as the artifact names it
// (dists/trixie/InRelease). A file name with "@" is not served.
func Convert(r *http.Request) (reference.Reference, map[string]string, error) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	repo, rest, ok := strings.Cut(p, ":")
	if i := strings.Index(p, "@"); i >= 0 && !strings.Contains(p[:i], ":") {
		repo, rest, ok = p[:i], p[i+1:], true
	}
	if !ok || len(repo) == 0 {
		return reference.Reference{}, nil, fmt.Errorf("%s: a path is /<repo>:<tag>/<file> or /<repo>@<digest>/<file>", r.URL.Path)
	}
	tag, file, ok := strings.Cut(rest, "/")
	if !ok || len(tag) == 0 || len(file) == 0 {
		return reference.Reference{}, nil, fmt.Errorf("%s: a path is /<repo>:<tag>/<file> or /<repo>@<digest>/<file>", r.URL.Path)
	}
	opts := map[string]string{ocispecv1.AnnotationTitle: file}
	if platform := r.URL.Query().Get("platform"); len(platform) > 0 {
		opts["platform"] = platform
	}
	return reference.Reference{Path: "/" + repo, Ref: tag}, opts, nil
}
