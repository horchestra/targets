package host

import (
	"fmt"
	"strings"

	"github.com/horchestra/pack/pkg/render"
)

// keep stores the step's credentials in the state; one the store already holds where the credential says
// (c.Store) need not be written again.
func (in *Installation) keep(creds []render.Credential) error {
	for _, c := range creds {
		if len(c.Store) > 0 {
			continue
		}
		if err := in.state.keep(c.Name, c.Leaf, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// resolver reads ${secret:<name>/<key>}: the step's credentials, else the ones kept in the state, else the
// environment's secrets_<name>_<key> (its file, its Vault KV).
func (in *Installation) resolver(creds []render.Credential) func(string) (string, error) {
	return func(ref string) (string, error) {
		name, key, ok := strings.Cut(ref, "/")
		if !ok {
			return "", fmt.Errorf("${secret:%s}: <name>/<key>", ref)
		}
		for _, c := range creds {
			if c.Name == name && c.Leaf == key {
				return c.Value, nil
			}
		}
		if v, ok, err := in.state.secret(name, key); err != nil || ok {
			return v, err
		}
		setting := strings.ToLower("secrets_" + name + "_" + key)
		if v, ok := in.settings[setting]; ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %s/%s: not kept in the state, and the environment has no %s", name, key, setting)
	}
}

// substitute replaces ${secret:<name>/<key>} with what resolve says.
func substitute(s string, resolve func(string) (string, error)) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${secret:")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		v, err := resolve(s[i+len("${secret:") : i+j])
		if err != nil {
			return "", err
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+1:]
	}
}
