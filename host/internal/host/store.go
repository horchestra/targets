package host

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/horchestra/pack/pkg/secretstore"
)

// storeState keeps the credentials — what a job publishes, what pack generated — in the environment's secret
// store, the record where the state kept it (nil under pack's `state: db`: the record is pack's database's). The
// store is shared: a key on another target reads what a job here published, and the other way round. A credential
// is the secret <prefix>/<name>, its key a field of it.
type storeState struct {
	state
	store        secretstore.Store
	cfg          secretstore.Config
	installation string
	// mu serializes keep's read-modify-write of a secret: steps run side by side, and KV v2 has no CAS
	mu sync.Mutex
}

func (s *storeState) path(name string) string { return s.cfg.PathOf(s.installation, name) }

func (s *storeState) noRecord() error {
	return fmt.Errorf("pack keeps the record in its state database (state: db): the target keeps none")
}

func (s *storeState) read() ([]byte, bool, error) {
	if s.state == nil {
		return nil, false, s.noRecord()
	}
	return s.state.read()
}

func (s *storeState) write(record []byte) error {
	if s.state == nil {
		return s.noRecord()
	}
	return s.state.write(record)
}

func (s *storeState) remove() error {
	if s.state == nil {
		return s.noRecord()
	}
	return s.state.remove()
}

func (s *storeState) secret(name, key string) (string, bool, error) {
	fields, ok, err := s.store.Get(context.Background(), s.path(name))
	if err != nil {
		return "", false, err
	}
	if v, has := fields[key]; ok && has {
		return v, true, nil
	}
	if s.state == nil {
		return "", false, nil
	}
	return s.state.secret(name, key) // one kept before the store was plugged in
}

func (s *storeState) keep(name, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(name)
	fields, _, err := s.store.Get(context.Background(), path)
	if err != nil {
		return err
	}
	if fields[key] == value {
		return nil
	}
	next := map[string]string{}
	maps.Copy(next, fields)
	next[key] = value
	return s.store.Put(context.Background(), path, next)
}

// usesStore: the credentials live in the store.
func (in *Installation) usesStore() bool {
	_, ok := in.state.(*storeState)
	return ok
}

// withStore plugs the store the environment names into the state: the default place of the credentials a job
// makes — a file of the target's would not outlive a job.
func (in *Installation) withStore() error {
	cfg := secretstore.FromSettings(func(k string) string { return in.settings.Get(k, "") })
	st, err := secretstore.Open(cfg)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if st != nil {
		in.state = &storeState{state: in.state, store: st, cfg: cfg, installation: in.installation}
	}
	return nil
}
