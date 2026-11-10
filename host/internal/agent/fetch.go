package agent

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// fetchClient is the agent's HTTP client: the downloads land through it, no curl or wget of the host's is asked
// for.
var fetchClient = &http.Client{Timeout: 10 * time.Minute}

// Fetch downloads the URL to the path and says whether that changed it: to a temporary file, compared with what
// is there, renamed when it differs.
func (s *Server) Fetch(ctx context.Context, req *agentpb.FetchRequest) (*agentpb.FetchResponse, error) {
	if len(req.Path) == 0 || req.Path[0] != '/' {
		return nil, status.Error(codes.InvalidArgument, "fetch: an absolute path")
	}
	if len(req.Url) == 0 {
		return nil, status.Error(codes.InvalidArgument, "fetch: no url")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.Url, nil)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "fetch %s: %v", req.Url, err)
	}
	resp, err := fetchClient.Do(hreq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "fetch %s: %s", req.Url, resp.Status)
	}
	mode := fs.FileMode(req.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	tmp, err := os.CreateTemp(dir, ".pack-fetch-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	if sameFile(tmp.Name(), req.Path, mode) {
		return &agentpb.FetchResponse{}, nil
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	if err := os.Rename(tmp.Name(), req.Path); err != nil {
		return nil, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	return &agentpb.FetchResponse{Changed: true}, nil
}

// sameFile: the path is already exactly the download, mode included.
func sameFile(tmp, path string, mode fs.FileMode) bool {
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != mode {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	n, err := os.ReadFile(tmp)
	if err != nil {
		return false
	}
	return bytes.Equal(b, n)
}
