package agent

import (
	"crypto/sha256"
	"errors"
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
	if len(first.Path) == 0 || first.Path[0] != '/' {
		return status.Error(codes.InvalidArgument, "put: an absolute path")
	}
	mode := fs.FileMode(first.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(first.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	tmp, err := os.CreateTemp(dir, ".pack-put-*")
	if err != nil {
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if err := writeChunks(w, first.Chunk, stream); err != nil {
		_ = tmp.Close()
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	if sameDigest(h, first.Path, mode) {
		return stream.SendAndClose(&agentpb.PutFileResponse{})
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	if err := os.Rename(tmp.Name(), first.Path); err != nil {
		return status.Errorf(codes.Internal, "put %s: %v", first.Path, err)
	}
	return stream.SendAndClose(&agentpb.PutFileResponse{Changed: true})
}

func writeChunks(w io.Writer, first []byte, stream agentpb.Agent_PutFileServer) error {
	if _, err := w.Write(first); err != nil {
		return err
	}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(req.Chunk); err != nil {
			return err
		}
	}
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
