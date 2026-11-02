package k8s

import (
	"io/fs"
	"testing"

	"github.com/horchestra/pack/pkg/render"
)

// adh is a small release in the shape the tests need: zookeeper, three pods with a per-instance file, and kafka, three
// pods that read a credential through a config file and a keystore a job published.
func adh(t *testing.T) (*Installation, *render.Release) {
	t.Helper()
	inst, err := Factory("prod", map[string]string{"namespace": "data"})
	if err != nil {
		t.Fatal(err)
	}
	in := inst.(*Installation)
	mode := fs.FileMode(0o640)
	zk := render.Unit{
		Key: "zookeeper", Component: "zookeeper", Replicas: 3, Image: "images/zookeeper:3.9",
		Ports: []render.Port{{Name: "client", Port: 2181}},
		Data:  []render.Claim{{Name: "data", Path: "/var/lib/zookeeper", Size: "10Gi"}},
		Files: []render.File{
			{Path: "/etc/zookeeper/zoo.cfg", Mode: &mode, Content: []byte("tickTime=2000\n")},
			{Path: "/var/lib/zookeeper/myid", Instances: [][]byte{[]byte("1\n"), []byte("2\n"), []byte("3\n")}},
		},
		Readiness: &render.Readiness{TCP: &render.TCPProbe{Port: 2181}},
	}
	kafka := render.Unit{
		Key: "kafka", Component: "kafka", Replicas: 3, Image: "images/kafka:3.7",
		Ports: []render.Port{{Name: "client", Port: 9092}},
		Env:   []render.EnvVar{{Name: "ZK", Value: "prod-zookeeper-0.prod-zookeeper.data.svc:2181"}},
		Files: []render.File{{Path: "/opt/kafka/config/kafka_jaas.conf", Content: []byte("password=\"${secret:prod-kafka-sasl/password}\"\n"),
			Secrets: []string{"prod-kafka-sasl/password"}}},
		SecretFiles: []render.SecretFile{{Path: "/etc/kafka/keystore.jks", Secret: render.SecretRef{Name: "prod-kafka-tls", Key: "keystore.jks"}}},
	}
	rel := &render.Release{Installation: "prod", Units: []render.Unit{zk, kafka}}
	for _, k := range []string{"zookeeper", "kafka"} {
		three := 3
		if _, err := in.Resolve(k, render.Placement{Replicas: &three}); err != nil {
			t.Fatal(err)
		}
	}
	return in, rel
}
