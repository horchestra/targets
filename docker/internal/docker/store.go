package docker

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/secretstore"
)

// storeState keeps the credentials — what a job publishes, what pack generated — in the environment's secret
// store, and the record where the state kept it. The store is shared: a key on another target reads what a job here
// published, and the other way round. A credential is the secret <prefix>/<name>, its key a field of it.
type storeState struct {
	state
	store        secretstore.Store
	cfg          secretstore.Config
	installation string
	// mu serializes keep's read-modify-write of a secret: steps run side by side, and KV v2 has no CAS
	mu sync.Mutex
}

func (s *storeState) path(name string) string { return s.cfg.PathOf(s.installation, name) }

func (s *storeState) secret(ctx context.Context, name, key string) (string, bool, error) {
	fields, ok, err := s.store.Get(ctx, s.path(name))
	if err != nil {
		return "", false, err
	}
	if v, has := fields[key]; ok && has {
		return v, true, nil
	}
	return s.state.secret(ctx, name, key) // one kept before the store was plugged in
}

func (s *storeState) keep(ctx context.Context, name, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(name)
	fields, _, err := s.store.Get(ctx, path)
	if err != nil {
		return err
	}
	if fields[key] == value {
		return nil
	}
	next := map[string]string{}
	maps.Copy(next, fields)
	next[key] = value
	return s.store.Put(ctx, path, next)
}

// usesStore: the credentials live in the store.
func (in *Installation) usesStore() bool {
	_, ok := in.state.(*storeState)
	return ok
}

// withStore plugs the store the environment names into the state.
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

// kept: a credential the store already holds where the step's credentials say it is need not be written again.
func kept(c render.Credential) bool { return len(c.Store) > 0 }
