package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/horchestra/pack/pkg/render"
	"github.com/horchestra/pack/pkg/saga"
)

func recordValues(t *testing.T, value string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(render.Values{Values: map[string]any{"value": value}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func valueDigest(values json.RawMessage) string {
	digest := sha256.Sum256(values)
	return hex.EncodeToString(digest[:])
}

func TestRecordValuesOutgrowTheMainConfigMap(t *testing.T) {
	in := &Installation{installation: "prod", namespace: "default"}
	c := fake(in)
	large := recordValues(t, string(make([]byte, 128*1024)))
	record := &saga.Record{
		Installation: saga.Installation{Pack: "pack", Version: "2", Values: large},
		Previous:     &saga.Installation{Pack: "pack", Version: "1", Values: large},
	}
	for i := range 25 {
		record.History = append(record.History, saga.Revision{
			N: i + 1, Installation: saga.Installation{Pack: "pack", Version: fmt.Sprint(i), Values: large}, Outcome: saga.RevInstalled,
		})
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.WriteInstalled(raw); err != nil {
		t.Fatal(err)
	}
	main := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), key(in, in.recordName()), main); err != nil {
		t.Fatal(err)
	}
	if len(main.Data["record.json"]) >= len(raw) {
		t.Fatalf("the main record was not reduced: %d bytes, original %d", len(main.Data["record.json"]), len(raw))
	}
	read, exists, err := in.ReadInstalled()
	if err != nil || !exists {
		t.Fatal(err)
	}
	var restored saga.Record
	if err := json.Unmarshal(read, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.History) != 25 || restored.Previous == nil {
		t.Fatalf("history was not restored: %+v", restored)
	}
	for _, revision := range restored.History {
		if string(revision.Values) != string(large) {
			t.Fatalf("revision %d values differ", revision.N)
		}
	}
}

func TestRecordRevisionGarbageCollection(t *testing.T) {
	in := &Installation{installation: "prod", namespace: "default"}
	c := fake(in)
	firstValues, secondValues := recordValues(t, "first"), recordValues(t, "second")
	write := func(values ...json.RawMessage) {
		t.Helper()
		record := &saga.Record{Installation: saga.Installation{Pack: "pack"}}
		for i, value := range values {
			record.History = append(record.History, saga.Revision{
				N: i + 1, Installation: saga.Installation{Pack: "pack", Values: value}, Outcome: saga.RevInstalled,
			})
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := in.WriteInstalled(raw); err != nil {
			t.Fatal(err)
		}
	}
	write(firstValues, secondValues)
	write(secondValues)
	revisions := &corev1.ConfigMapList{}
	if err := c.List(context.Background(), revisions, client.MatchingLabels{labelPart: "revision"}); err != nil {
		t.Fatal(err)
	}
	if len(revisions.Items) != 1 || revisions.Items[0].Annotations[labelDigest] != valueDigest(secondValues) {
		t.Fatalf("the pruned revision was not collected: %+v", revisions.Items)
	}
}
