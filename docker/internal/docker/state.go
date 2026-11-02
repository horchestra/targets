package docker

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// state is where an installation's record and the credentials pack
// provisioned for it are kept: on the engine (engineState), or in a file
// the environment names (fileState, the setting state).
type state interface {
	read(ctx context.Context) (record []byte, exists bool, err error)
	write(ctx context.Context, record []byte) error
	// remove deletes the installation's record — pack delete's last act.
	remove(ctx context.Context) error
	// secret is a kept credential's value.
	secret(ctx context.Context, name, key string) (value string, ok bool, err error)
	// keep stores a credential, replacing a value that changed.
	keep(ctx context.Context, name, key, value string) error
}

const (
	labelRecord     = "pack.horchestra.io/record"
	labelGeneration = "pack.horchestra.io/generation"
	labelSecret     = "pack.horchestra.io/secret"
	labelSecretKey  = "pack.horchestra.io/secret-key"
	labelValue      = "pack.horchestra.io/value"
	// labelChunks: a value too long for one argument of docker's argv (the
	// kernel's MAX_ARG_STRLEN, 128 KiB — a CA bundle) is labelValue.0, .1, …
	labelChunks = "pack.horchestra.io/chunks"
	chunkSize   = 64 << 10
)

// engineState keeps the state on the engine, in volumes that hold nothing.
// The record is the label of pack-<installation>.<generation>, gzipped. A
// volume's labels are fixed, so a write makes the next generation and then
// removes the ones before it; a write cut short leaves two, and the newer is
// read. A credential is a volume of its own, the value in its label —
// readable by whoever can reach the engine, who is root on its host anyway.
// Across hosts it is the first host's engine, by name.
type engineState struct{ in *Installation }

func (s engineState) engine() *engine { return s.in.engines[0] }

func (s engineState) records(ctx context.Context) (gen int, names []string, record string, err error) {
	in := s.in
	names, err = s.engine().lines(ctx, append([]string{"volume", "ls", "-q"}, in.filters(labelPart+"=record")...)...)
	if err != nil || len(names) == 0 {
		return -1, nil, "", err
	}
	rows, err := s.engine().lines(ctx, append([]string{"volume", "inspect", "--format",
		`{{index .Labels "` + labelGeneration + `"}} {{index .Labels "` + labelRecord + `"}}`}, names...)...)
	if err != nil {
		return -1, nil, "", err
	}
	gen = -1
	for _, r := range rows {
		g, rec, _ := strings.Cut(r, " ")
		if n, err := strconv.Atoi(g); err == nil && n > gen {
			gen, record = n, rec
		}
	}
	return gen, names, record, nil
}

// recordBytes is the record a labelRecord label keeps: it, gzipped, in
// base64.
func recordBytes(rec string) ([]byte, error) {
	z, err := base64.StdEncoding.DecodeString(rec)
	if err != nil {
		return nil, err
	}
	r, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func (s engineState) read(ctx context.Context) ([]byte, bool, error) {
	gen, _, rec, err := s.records(ctx)
	if err != nil || gen < 0 || len(rec) == 0 {
		return nil, false, err
	}
	b, err := recordBytes(rec)
	return b, err == nil, err
}

// remove deletes the installation's record volumes, every generation. The
// credentials' volumes and the claims' data stay: pack delete keeps the
// data.
func (s engineState) remove(ctx context.Context) error {
	_, names, _, err := s.records(ctx)
	if err != nil || len(names) == 0 {
		return err
	}
	_, err = s.engine().docker(ctx, nil, nil, nil, append([]string{"volume", "rm"}, names...)...)
	return err
}

// listRecords is the record of every installation the engine keeps, by name
// — records() without the installation filter, for pack ps. Of one
// installation's generations the newest, as read reads it.
func listRecords(ctx context.Context, e *engine) (map[string][]byte, error) {
	names, err := e.lines(ctx, "volume", "ls", "-q", "--filter", "label="+labelPart+"=record")
	if err != nil || len(names) == 0 {
		return nil, err
	}
	rows, err := e.lines(ctx, append([]string{"volume", "inspect", "--format",
		`{{index .Labels "` + labelInstallation + `"}} {{index .Labels "` + labelGeneration + `"}} {{index .Labels "` + labelRecord + `"}}`},
		names...)...)
	if err != nil {
		return nil, err
	}
	type kept struct {
		gen    int
		record string
	}
	by := map[string]kept{}
	for _, r := range rows {
		f := strings.Fields(r)
		if len(f) != 3 {
			continue
		}
		gen, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		if k, ok := by[f[0]]; !ok || gen > k.gen {
			by[f[0]] = kept{gen, f[2]}
		}
	}
	records := map[string][]byte{}
	for name, k := range by {
		b, err := recordBytes(k.record)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", e.prefix(), name, err)
		}
		records[name] = b
	}
	return records, nil
}

func (s engineState) write(ctx context.Context, record []byte) error {
	in := s.in
	gen, old, _, err := s.records(ctx)
	if err != nil {
		return err
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write(record)
	_ = w.Close()
	next := gen + 1
	if _, err := s.engine().docker(ctx, nil, nil, nil, "volume", "create",
		"--label", labelInstallation+"="+in.installation, "--label", labelPart+"=record",
		"--label", labelGeneration+"="+strconv.Itoa(next),
		"--label", labelRecord+"="+base64.StdEncoding.EncodeToString(z.Bytes()),
		fmt.Sprintf("pack-%s.%d", in.installation, next)); err != nil {
		return err
	}
	if len(old) > 0 {
		_, err = s.engine().docker(ctx, nil, nil, nil, append([]string{"volume", "rm"}, old...)...)
	}
	return err
}

// secrets are the volumes that keep a credential, and the value of the first.
func (s engineState) secrets(ctx context.Context, name, key string) (string, []string, error) {
	in := s.in
	names, err := s.engine().lines(ctx, append([]string{"volume", "ls", "-q"},
		in.filters(labelSecret+"="+name, labelSecretKey+"="+key)...)...)
	if err != nil || len(names) == 0 {
		return "", nil, err
	}
	raw, err := s.engine().docker(ctx, nil, nil, nil, "volume", "inspect", "--format", `{{json .Labels}}`, names[0])
	if err != nil {
		return "", nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal(raw, &labels); err != nil {
		return "", nil, err
	}
	enc := labels[labelValue]
	if n, err := strconv.Atoi(labels[labelChunks]); err == nil {
		var b strings.Builder
		for i := range n {
			b.WriteString(labels[labelValue+"."+strconv.Itoa(i)])
		}
		enc = b.String()
	}
	v, err := base64.StdEncoding.DecodeString(enc)
	return string(v), names, err
}

// valueLabels are the labels that keep a credential's value: one, or the
// chunks of one too long for an argument.
func valueLabels(value string) []string {
	enc := base64.StdEncoding.EncodeToString([]byte(value))
	if len(enc) <= chunkSize {
		return []string{"--label", labelValue + "=" + enc}
	}
	var args []string
	n := 0
	for ; len(enc) > 0; n++ {
		c := enc[:min(chunkSize, len(enc))]
		enc = enc[len(c):]
		args = append(args, "--label", labelValue+"."+strconv.Itoa(n)+"="+c)
	}
	return append(args, "--label", labelChunks+"="+strconv.Itoa(n))
}

func (s engineState) secret(ctx context.Context, name, key string) (string, bool, error) {
	v, names, err := s.secrets(ctx, name, key)
	return v, len(names) > 0, err
}

func (s engineState) keep(ctx context.Context, name, key, value string) error {
	in := s.in
	val, names, err := s.secrets(ctx, name, key)
	if err != nil || (len(names) == 1 && val == value) {
		return err
	}
	if len(names) > 0 {
		if _, err := s.engine().docker(ctx, nil, nil, nil, append([]string{"volume", "rm"}, names...)...); err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(in.installation + "/" + name + "/" + key))
	args := append([]string{"volume", "create",
		"--label", labelInstallation + "=" + in.installation, "--label", labelPart + "=secret",
		"--label", labelSecret + "=" + name, "--label", labelSecretKey + "=" + key}, valueLabels(value)...)
	_, err = s.engine().docker(ctx, nil, nil, nil, append(args,
		containerName("pack-secret-"+name+"-"+hex.EncodeToString(sum[:4])))...)
	return err
}

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

// list is the record of every installation the file keeps — pack ps on a
// fileState.
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
// credentials kept for it stay, as they do on an engine: pack delete keeps
// the data.
func (s *fileState) remove(context.Context) error {
	return s.updateFile(func(f *stateFile) bool {
		st := f.Installations[s.installation]
		if st == nil || len(st.Record) == 0 {
			return false
		}
		st.Record = nil
		return true
	})
}

func (s *fileState) read(context.Context) ([]byte, bool, error) {
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

func (s *fileState) write(_ context.Context, record []byte) error {
	if !json.Valid(record) {
		return fmt.Errorf("state %s: the record is not JSON", s.path)
	}
	return s.update(func(st *installationState) bool {
		st.Record = json.RawMessage(record)
		return true
	})
}

func (s *fileState) secret(_ context.Context, name, key string) (string, bool, error) {
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

func (s *fileState) keep(_ context.Context, name, key, value string) error {
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
