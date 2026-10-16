package k8s

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
	StringData map[string]string `yaml:"stringData"`
	Spec       struct {
		VolumeClaimTemplates []struct {
			Spec struct {
				StorageClassName string `yaml:"storageClassName"`
			} `yaml:"spec"`
		} `yaml:"volumeClaimTemplates"`
	} `yaml:"spec"`
}

var clusterScoped = map[string]bool{"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true}

func render(t *testing.T, cfg config.Config, opts Options) []object {
	t.Helper()
	out, err := Render(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	var objs []object
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return objs
		}
		if err != nil {
			t.Fatalf("invalid YAML in rendered manifests: %v", err)
		}
		objs = append(objs, o)
	}
}

func TestRender(t *testing.T) {
	cfg := config.Config{TelegramToken: "1:a", TelegramChatID: "2", GrafanaPassword: `p"w: #x`}
	objs := render(t, cfg, Options{Namespace: "mon", StorageClass: "fast"})

	if objs[0].Kind != "Namespace" || objs[0].Metadata.Name != "mon" {
		t.Errorf("first object = %s/%s, want Namespace/mon", objs[0].Kind, objs[0].Metadata.Name)
	}
	byKey := map[string]object{}
	for _, o := range objs {
		if o.APIVersion == "" || o.Kind == "" || o.Metadata.Name == "" {
			t.Errorf("incomplete object: %+v", o)
		}
		if clusterScoped[o.Kind] != (o.Metadata.Namespace == "") {
			t.Errorf("%s/%s has namespace %q", o.Kind, o.Metadata.Name, o.Metadata.Namespace)
		}
		if !clusterScoped[o.Kind] && o.Metadata.Namespace != "mon" {
			t.Errorf("%s/%s in namespace %q, want mon", o.Kind, o.Metadata.Name, o.Metadata.Namespace)
		}
		for _, v := range o.Spec.VolumeClaimTemplates {
			if v.Spec.StorageClassName != "fast" {
				t.Errorf("%s/%s storageClassName = %q", o.Kind, o.Metadata.Name, v.Spec.StorageClassName)
			}
		}
		key := o.Kind + "/" + o.Metadata.Name
		if _, dup := byKey[key]; dup {
			t.Errorf("duplicate object %s", key)
		}
		byKey[key] = o
	}

	for _, k := range []string{
		"StatefulSet/prometheus", "Deployment/alertmanager", "StatefulSet/grafana",
		"DaemonSet/node-exporter", "Deployment/blackbox", "StatefulSet/loki",
		"DaemonSet/fluent-bit", "StatefulSet/jaeger", "ClusterRole/mon-prometheus",
	} {
		if _, ok := byKey[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}

	// Embedded files must survive YAML round-tripping intact.
	if pw := byKey["Secret/grafana-admin"].StringData["password"]; pw != cfg.GrafanaPassword {
		t.Errorf("grafana password = %q, want %q", pw, cfg.GrafanaPassword)
	}
	prom := byKey["ConfigMap/prometheus-config"].Data
	if !strings.Contains(prom["prometheus.yml"], "names: ['mon']") {
		t.Errorf("prometheus.yml not scoped to namespace:\n%s", prom["prometheus.yml"])
	}
	if !strings.Contains(prom["rules.yml"], "{{ $labels.instance }}") {
		t.Error("rules.yml lost its alert templates")
	}
	if !strings.Contains(byKey["Secret/alertmanager-config"].StringData["alertmanager.yml"], "chat_id: 2") {
		t.Error("alertmanager config not rendered")
	}
	dash := byKey["ConfigMap/grafana-dashboards"].Data
	for _, k := range []string{"dashboard.yml", "node-exporter-full.json", "cadvisor.json"} {
		if dash[k] == "" {
			t.Errorf("dashboards ConfigMap missing %s", k)
		}
	}
}

func TestRenderNoStorageClass(t *testing.T) {
	out, err := Render(config.Config{GrafanaPassword: "x"}, Options{Namespace: "infra-monitor"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("storageClassName")) {
		t.Error("storageClassName set without --storage-class")
	}
}

func TestRenderSkipNamespace(t *testing.T) {
	for _, o := range render(t, config.Config{}, Options{Namespace: "mon", SkipNamespace: true}) {
		if o.Kind == "Namespace" {
			t.Error("Namespace rendered with SkipNamespace")
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	for _, o := range []Options{
		{Namespace: ""},
		{Namespace: "Bad_NS"},
		{Namespace: "ok", StorageClass: "x\ny: z"},
	} {
		if o.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", o)
		}
	}
}

func TestConfigChangeRollsPods(t *testing.T) {
	a, _ := Render(config.Config{GrafanaPassword: "x"}, Options{Namespace: "n"})
	b, _ := Render(config.Config{GrafanaPassword: "x", TelegramToken: "1:a", TelegramChatID: "2"}, Options{Namespace: "n"})
	sum := func(out []byte) string {
		i := bytes.Index(out, []byte("kind: Deployment\nmetadata:\n  name: alertmanager"))
		j := bytes.Index(out[i:], []byte("checksum/config: "))
		return string(out[i+j : i+j+40])
	}
	if sum(a) == sum(b) {
		t.Error("alertmanager checksum unchanged after config change")
	}
}
