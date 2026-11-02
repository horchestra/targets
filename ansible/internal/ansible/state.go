package ansible

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// fileState keeps the state in a file, JSON, the installations it holds by
// name — one file serves every installation of an environment:
//
//	{"installations": {"prod": {"record": {…}, "secrets": {"<name>/<key>": "…"}}}}
//
// It holds credentials in plain text: 0600, in a directory made 0700. A
// write takes an exclusive lock on <file>.lock, reads, changes and replaces
// the file by rename — a reader sees the old file or the new one.
type fileState struct{ path, installation string }

type stateFile struct {
	Installations map[string]*installationState `json:"installations"`
}

type installationState struct {
	Record  json.RawMessage   `json:"record,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// Binary are the credentials that are no UTF-8 text — a keytab, a
	// keystore a job published — base64: a JSON string would mangle them.
	Binary map[string]string `json:"binary,omitempty"`
}

// newFileState is the state in path; ~/ is the home directory, a relative
// path is the provider's working directory's.
func newFileState(path, installation string) (*fileState, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("state %s: %w", path, err)
		}
		path = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	return &fileState{path: abs, installation: installation}, nil
}

func (s *fileState) load() (*stateFile, error) {
	f := &stateFile{Installations: map[string]*installationState{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("state %s: %w", s.path, err)
	}
	if f.Installations == nil {
		f.Installations = map[string]*installationState{}
	}
	return f, nil
}

// updateFile changes the file under the lock and writes it back.
func (s *fileState) updateFile(change func(*stateFile) bool) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("state %s: lock: %w", s.path, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	f, err := s.load()
	if err != nil {
		return err
	}
	if !change(f) {
		return nil
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// update changes the installation's entry under the lock and writes the file.
func (s *fileState) update(change func(*installationState) bool) error {
	return s.updateFile(func(f *stateFile) bool {
		st := f.Installations[s.installation]
		if st == nil {
			st = &installationState{}
			f.Installations[s.installation] = st
		}
		return change(st)
	})
}

// list is the record of every installation the file keeps — pack ps.
func (s *fileState) list() (map[string][]byte, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	records := map[string][]byte{}
	for name, st := range f.Installations {
		if len(st.Record) == 0 {
			continue
		}
		var b bytes.Buffer
		if err := json.Compact(&b, st.Record); err != nil {
			return nil, err
		}
		records[name] = b.Bytes()
	}
	return records, nil
}

// remove drops the installation's record — pack delete's last act. The
// credentials kept for it stay, as the data does.
func (s *fileState) remove() error {
	return s.updateFile(func(f *stateFile) bool {
		st := f.Installations[s.installation]
		if st == nil || len(st.Record) == 0 {
			return false
		}
		st.Record = nil
		return true
	})
}

func (s *fileState) read() ([]byte, bool, error) {
	f, err := s.load()
	if err != nil {
		return nil, false, err
	}
	st := f.Installations[s.installation]
	if st == nil || len(st.Record) == 0 {
		return nil, false, nil
	}
	// the file is indented for reading; the record is given back compact, as
	// the saga marshals it
	var b bytes.Buffer
	if err := json.Compact(&b, st.Record); err != nil {
		return nil, false, err
	}
	return b.Bytes(), true, nil
}

func (s *fileState) write(record []byte) error {
	if !json.Valid(record) {
		return fmt.Errorf("state %s: the record is not JSON", s.path)
	}
	return s.update(func(st *installationState) bool {
		st.Record = json.RawMessage(record)
		return true
	})
}

func (s *fileState) secret(name, key string) (string, bool, error) {
	f, err := s.load()
	if err != nil {
		return "", false, err
	}
	st := f.Installations[s.installation]
	if st == nil {
		return "", false, nil
	}
	if b, ok := st.Binary[name+"/"+key]; ok {
		v, err := base64.StdEncoding.DecodeString(b)
		return string(v), err == nil, err
	}
	v, ok := st.Secrets[name+"/"+key]
	return v, ok, nil
}

func (s *fileState) keep(name, key, value string) error {
	ref, text := name+"/"+key, utf8.ValidString(value)
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	return s.update(func(st *installationState) bool {
		if v, ok := st.Secrets[ref]; ok && text && v == value {
			return false
		}
		if v, ok := st.Binary[ref]; ok && !text && v == enc {
			return false
		}
		delete(st.Secrets, ref)
		delete(st.Binary, ref)
		if text {
			if st.Secrets == nil {
				st.Secrets = map[string]string{}
			}
			st.Secrets[ref] = value
		} else {
			if st.Binary == nil {
				st.Binary = map[string]string{}
			}
			st.Binary[ref] = enc
		}
		return true
	})
}
