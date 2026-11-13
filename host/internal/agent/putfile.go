package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/horchestra/targets/host/internal/agentpb"
)

// PutFile writes the stream's chunks to the path, its directory made first, atomically (a temporary file
// renamed), and says whether anything changed. The bytes are compared as they come: the file is never held whole.
func (s *Server) PutFile(stream agentpb.Agent_PutFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "put: %v", err)
	}
	reader := io.MultiReader(bytes.NewReader(first.Chunk), &putStreamReader{stream: stream})
	changed, err := s.Put(stream.Context(), first.Path, first.Mode, reader)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&agentpb.PutFileResponse{Changed: changed})
}

// Put writes a stream directly to a file without a gRPC transport.
func (s *Server) Put(_ context.Context, path string, fileMode uint32, reader io.Reader) (bool, error) {
	if len(path) == 0 || path[0] != '/' {
		return false, status.Error(codes.InvalidArgument, "put: an absolute path")
	}
	mode := fs.FileMode(fileMode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	tmp, err := os.CreateTemp(dir, ".pack-put-*")
	if err != nil {
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if _, err := io.Copy(w, reader); err != nil {
		_ = tmp.Close()
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	if sameDigest(h, path, mode) {
		return false, nil
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, status.Errorf(codes.Internal, "put %s: %v", path, err)
	}
	return true, nil
}

type putStreamReader struct {
	stream  agentpb.Agent_PutFileServer
	pending []byte
}

func (r *putStreamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		req, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		r.pending = req.Chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// sameDigest: the path is already exactly the streamed bytes — their digest h was taken as the chunks went by,
// the mode is checked too.
func sameDigest(h hash.Hash, path string, mode fs.FileMode) bool {
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != mode {
		return false
	}
	cur, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = cur.Close() }()
	existing := sha256.New()
	if _, err := io.Copy(existing, cur); err != nil {
		return false
	}
	return string(existing.Sum(nil)) == string(h.Sum(nil))
}
