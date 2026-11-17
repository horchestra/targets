package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const labelDigest = "pack.horchestra.io/digest"
const valuesReferenceKey = "$pack_values"

func externalizeValues(values json.RawMessage) (json.RawMessage, string, error) {
	if len(values) == 0 {
		return nil, "", nil
	}
	digest := sha256.Sum256(values)
	name := hex.EncodeToString(digest[:])
	reference, err := json.Marshal(map[string]string{valuesReferenceKey: name})
	return reference, name, err
}

func restoreValues(values json.RawMessage) (string, bool, error) {
	if len(values) == 0 {
		return "", false, nil
	}
	var reference map[string]string
	if err := json.Unmarshal(values, &reference); err != nil {
		return "", false, nil
	}
	digest, ok := reference[valuesReferenceKey]
	if !ok || len(reference) != 1 {
		return "", false, nil
	}
	if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size {
		return "", false, fmt.Errorf("values reference %q is not sha256", digest)
	}
	return digest, true, nil
}

func nestedObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

// externalizeRecord moves every values document into a content-addressed revision ConfigMap without dropping raw fields.
func (in *Installation) externalizeRecord(raw []byte) ([]byte, map[string]*corev1.ConfigMap, error) {
	record, err := nestedObject(raw)
	if err != nil {
		return nil, nil, err
	}
	revisions := map[string]*corev1.ConfigMap{}
	externalize := func(object map[string]json.RawMessage) error {
		if object == nil {
			return nil
		}
		values := object["values"]
		reference, digest, err := externalizeValues(values)
		if err != nil || len(digest) == 0 {
			return err
		}
		revisions[digest] = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: in.recordName() + "-" + digest[:16], Namespace: in.namespace,
				Labels:      map[string]string{labelInstance: in.installation, labelManagedBy: "pack", labelPart: "revision"},
				Annotations: map[string]string{labelDigest: digest},
			},
			Data: map[string]string{"values.json": string(values)},
		}
		object["values"] = reference
		return nil
	}
	if err := externalize(record); err != nil {
		return nil, nil, err
	}
	previous, err := nestedObject(record["previous"])
	if err != nil {
		return nil, nil, err
	}
	if err := externalize(previous); err != nil {
		return nil, nil, err
	}
	if previous != nil {
		if record["previous"], err = json.Marshal(previous); err != nil {
			return nil, nil, err
		}
	}
	var history []json.RawMessage
	if len(record["history"]) > 0 {
		if err := json.Unmarshal(record["history"], &history); err != nil {
			return nil, nil, err
		}
	}
	for i := range history {
		revision, err := nestedObject(history[i])
		if err != nil {
			return nil, nil, err
		}
		if err := externalize(revision); err != nil {
			return nil, nil, err
		}
		if history[i], err = json.Marshal(revision); err != nil {
			return nil, nil, err
		}
	}
	if history != nil {
		if record["history"], err = json.Marshal(history); err != nil {
			return nil, nil, err
		}
	}
	out, err := json.Marshal(record)
	return out, revisions, err
}

func (in *Installation) restoreRecord(ctx context.Context, raw []byte) ([]byte, error) {
	record, err := nestedObject(raw)
	if err != nil {
		return nil, err
	}
	c, err := in.cluster()
	if err != nil {
		return nil, err
	}
	cache := map[string]json.RawMessage{}
	restore := func(object map[string]json.RawMessage) error {
		if object == nil {
			return nil
		}
		digest, ok, err := restoreValues(object["values"])
		if err != nil || !ok {
			return err
		}
		values, cached := cache[digest]
		if !cached {
			cm := &corev1.ConfigMap{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: in.recordName() + "-" + digest[:16]}, cm); err != nil {
				return err
			}
			values = json.RawMessage(cm.Data["values.json"])
			cache[digest] = values
		}
		object["values"] = values
		return nil
	}
	if err := restore(record); err != nil {
		return nil, err
	}
	previous, err := nestedObject(record["previous"])
	if err != nil {
		return nil, err
	}
	if err := restore(previous); err != nil {
		return nil, err
	}
	if previous != nil {
		if record["previous"], err = json.Marshal(previous); err != nil {
			return nil, err
		}
	}
	var history []json.RawMessage
	if len(record["history"]) > 0 {
		if err := json.Unmarshal(record["history"], &history); err != nil {
			return nil, err
		}
	}
	for i := range history {
		revision, err := nestedObject(history[i])
		if err != nil {
			return nil, err
		}
		if err := restore(revision); err != nil {
			return nil, err
		}
		if history[i], err = json.Marshal(revision); err != nil {
			return nil, err
		}
	}
	if history != nil {
		if record["history"], err = json.Marshal(history); err != nil {
			return nil, err
		}
	}
	return json.Marshal(record)
}

func (in *Installation) writeRevisions(ctx context.Context, revisions map[string]*corev1.ConfigMap) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	for _, revision := range revisions {
		if err := c.Create(ctx, revision); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func (in *Installation) pruneRevisions(ctx context.Context, keep map[string]*corev1.ConfigMap) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	list := &corev1.ConfigMapList{}
	selector := client.MatchingLabels{labelInstance: in.installation, labelManagedBy: "pack", labelPart: "revision"}
	if err := c.List(ctx, list, client.InNamespace(in.namespace), selector); err != nil {
		return err
	}
	for i := range list.Items {
		revision := &list.Items[i]
		if digest := revision.Annotations[labelDigest]; keep[digest] == nil {
			if err := c.Delete(ctx, revision); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
