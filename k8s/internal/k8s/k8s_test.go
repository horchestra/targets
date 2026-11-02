package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/target"
)

func TestSubstitute(t *testing.T) {
	got, err := substitute("user=a\npassword=${secret:pg/password}\n", func(ref string) (string, error) {
		if ref != "pg/password" {
			t.Errorf("ref %q", ref)
		}
		return "s3cret", nil
	})
	if err != nil || got != "user=a\npassword=s3cret\n" {
		t.Errorf("%q %v", got, err)
	}
}

// A setting of another target, or a typo, is refused.
func TestUnknownSetting(t *testing.T) {
	for _, s := range []target.Settings{{"network": "x"}, {"namspace": "data"}, {"hosts_a_address": "a"}} {
		if _, err := Factory("dev", s); err == nil {
			t.Errorf("%v accepted", s)
		}
	}
}

// With secret_delivery vault-csi no Secret of ours holds a credential the store has: the key gets a
// SecretProviderClass that names every secret its pod reads, and the pod mounts it.
func TestSecretsFromTheStore(t *testing.T) {
	in, rel := adh(t)
	var kafka render.Unit
	for _, u := range rel.Units {
		if u.Key == "kafka" {
			kafka = u
		}
	}
	creds := []render.Credential{{Name: "prod-kafka-sasl", Leaf: "password", Value: "v", Key: "kafka",
		Store: "team/kafka", Field: "sasl"}}

	has := func(objs []obj, kind string) bool {
		for _, o := range objs {
			if o["kind"] == kind {
				return true
			}
		}
		return false
	}
	native, err := in.objects(kafka, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if has(native, "SecretProviderClass") {
		t.Error("native: no SecretProviderClass")
	}

	ext := &Installation{installation: in.installation, namespace: in.namespace}
	ext.settings = map[string]string{"secret_delivery": "vault-csi", "vault_role": "pack", "vault_addr": "https://bao.example:8200",
		"vault_mount": "kv", "store_prefix": "pack/prod"}
	objs, err := ext.objects(kafka, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	var spc obj
	secretNames := map[string]bool{}
	for _, o := range objs {
		switch o["kind"] {
		case "SecretProviderClass":
			spc = o
		case "Secret":
			secretNames[o["metadata"].(obj)["name"].(string)] = true
		}
	}
	if spc == nil {
		t.Fatalf("no SecretProviderClass in %d objects", len(objs))
	}
	if secretNames["prod-kafka-sasl"] {
		t.Error("the credential the store holds is no Secret of ours")
	}
	spec := spc["spec"].(obj)
	params := spec["parameters"].(obj)
	if spec["provider"] != "vault" || params["roleName"] != "pack" || params["vaultAddress"] != "https://bao.example:8200" ||
		!strings.Contains(params["objects"].(string), `secretPath: "kv/data/pack/prod/prod-kafka-tls"`) || // where a job publishes it
		!strings.Contains(params["objects"].(string), `secretKey: "keystore.jks"`) {
		t.Errorf("%+v", spec)
	}
	var sts obj
	for _, o := range objs {
		if o["kind"] == "StatefulSet" {
			sts = o
		}
	}
	pod := sts["spec"].(obj)["template"].(obj)["spec"].(obj)
	found := false
	for _, v := range pod["volumes"].([]any) {
		if vol := v.(obj); vol["name"] == "pack-secrets" {
			found = vol["csi"].(obj)["volumeAttributes"].(obj)["secretProviderClass"] == "prod-kafka-secrets"
		}
	}
	if !found {
		t.Errorf("the pod mounts the class: %+v", pod["volumes"])
	}

	// refusals: no role; the injector, which is not done
	ext.settings = map[string]string{"secret_delivery": "vault-csi", "vault_addr": "x"}
	if _, err := ext.objects(kafka, creds, nil); err == nil || !strings.Contains(err.Error(), "vault_role") {
		t.Errorf("no role: %v", err)
	}
	if _, err := Factory("prod", target.Settings{"secret_delivery": "vault-injector"}); err == nil {
		t.Error("the injector is not done")
	}
	if _, err := Factory("prod", target.Settings{"secret_delivery": "nowhere"}); err == nil {
		t.Error("an unknown provider")
	}
}

// A ${secret:…} placeholder of a config file is read from the store when the store delivers the Secrets: where the
// credential says it is, else where a job publishes it.
func TestPlaceholdersFromTheStore(t *testing.T) {
	kv := map[string]map[string]any{
		"team/kafka":               {"sasl": "from-team"},
		"pack/prod/prod-kafka-tls": {"keystore.jks": "jks"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _ := strings.CutPrefix(r.URL.Path, "/v1/secret/data/")
		d, ok := kv[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": d}})
	}))
	defer srv.Close()
	in := &Installation{installation: "prod", namespace: "data", settings: map[string]string{
		"secret_delivery": "vault-csi", "store_provider": "openbao", "store_addr": srv.URL, "store_token": "t"}}
	creds := []render.Credential{{Name: "prod-kafka-sasl", Leaf: "password", Store: "team/kafka", Field: "sasl"}}
	resolve := in.resolver(context.Background(), creds)
	if v, err := resolve("prod-kafka-sasl/password"); err != nil || v != "from-team" {
		t.Errorf("where the credential says: %q %v", v, err)
	}
	if v, err := resolve("prod-kafka-tls/keystore.jks"); err != nil || v != "jks" {
		t.Errorf("where a job publishes: %q %v", v, err)
	}
	if _, err := resolve("prod-kafka-tls/nothing"); err == nil {
		t.Error("a field nobody wrote")
	}
	if _, err := resolve("nobody/x"); err == nil {
		t.Error("a secret nobody wrote")
	}
}
