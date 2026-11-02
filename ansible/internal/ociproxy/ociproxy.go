// Package ociproxy is what oci-proxy adds to oci-packer's proxy: the files of an artifact as an ordinary tree, what apt and
// dnf ask a repository for, with no query.
package ociproxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/arenadata/oci-packer/pkg/registry/reference"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Convert reads /<repo>:<tag>/<file>: the first colon ends the repository, the first slash after it the tag, the rest is
// the file as the artifact names it (dists/trixie/InRelease).
func Convert(r *http.Request) (reference.Reference, map[string]string, error) {
	repo, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ":")
	if !ok || len(repo) == 0 {
		return reference.Reference{}, nil, fmt.Errorf("%s: a path is /<repo>:<tag>/<file>", r.URL.Path)
	}
	tag, file, ok := strings.Cut(rest, "/")
	if !ok || len(tag) == 0 || len(file) == 0 {
		return reference.Reference{}, nil, fmt.Errorf("%s: a path is /<repo>:<tag>/<file>", r.URL.Path)
	}
	opts := map[string]string{ocispecv1.AnnotationTitle: file}
	if platform := r.URL.Query().Get("platform"); len(platform) > 0 {
		opts["platform"] = platform
	}
	return reference.Reference{Path: "/" + repo, Ref: tag}, opts, nil
}
