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
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, status.FromContextError(ctx.Err()).Err()
			case <-timer.C:
			}
		}
		changed, retry, err := s.fetch(ctx, req)
		if err == nil {
			return &agentpb.FetchResponse{Changed: changed}, nil
		}
		if !retry || attempt == 2 {
			return nil, err
		}
	}
	panic("unreachable")
}

func (s *Server) fetch(ctx context.Context, req *agentpb.FetchRequest) (bool, bool, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.Url, nil)
	if err != nil {
		return false, false, status.Errorf(codes.InvalidArgument, "fetch %s: %v", req.Url, err)
	}
	resp, err := fetchClient.Do(hreq)
	if err != nil {
		return false, true, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode >= http.StatusInternalServerError, status.Errorf(codes.Internal, "fetch %s: %s", req.Url, resp.Status)
	}
	mode := fs.FileMode(req.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(req.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, false, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	tmp, err := os.CreateTemp(dir, ".pack-fetch-*")
	if err != nil {
		return false, false, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return false, true, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	if err := tmp.Close(); err != nil {
		return false, false, status.Errorf(codes.Internal, "fetch %s: %v", req.Url, err)
	}
	if sameFile(tmp.Name(), req.Path, mode) {
		return false, false, nil
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, false, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	if err := os.Rename(tmp.Name(), req.Path); err != nil {
		return false, false, status.Errorf(codes.Internal, "fetch %s: %v", req.Path, err)
	}
	return true, false, nil
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
