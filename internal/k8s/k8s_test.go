package k8s

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

type container struct {
	Name      string   `yaml:"name"`
	Image     string   `yaml:"image"`
	Args      []string `yaml:"args"`
	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`
	Env []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
	StringData map[string]string `yaml:"stringData"`
	Spec       struct {
		Rules []struct {
			Host string `yaml:"host"`
			HTTP struct {
				Paths []struct {
					Backend struct {
						Service struct {
							Name string `yaml:"name"`
						} `yaml:"service"`
					} `yaml:"backend"`
				} `yaml:"paths"`
			} `yaml:"http"`
		} `yaml:"rules"`
		IngressClassName string `yaml:"ingressClassName"`
		TLS              []struct {
			Hosts      []string `yaml:"hosts"`
			SecretName string   `yaml:"secretName"`
		} `yaml:"tls"`
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
		VolumeClaimTemplates []struct {
			Spec struct {
				StorageClassName string `yaml:"storageClassName"`
				Resources        struct {
					Requests map[string]string `yaml:"requests"`
				} `yaml:"resources"`
			} `yaml:"spec"`
		} `yaml:"volumeClaimTemplates"`
	} `yaml:"spec"`
}

var clusterScoped = map[string]bool{"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true}

func testConfig() config.Config {
	c := config.Default()
	c.Grafana.AdminPassword = fake(`p"w: #x`)
	c.Notifications.Telegram = &config.Telegram{BotToken: "1:a", ChatID: "2"}
	c.Kubernetes.Namespace = "mon"
	c.Kubernetes.StorageClass = "fast"
	c.Kubernetes.Storage.Prometheus = "50Gi"
	c.Retention.Metrics = "30d"
	return c
}

func render(t *testing.T, cfg config.Config, extras stack.Extras, opts Options) map[string]object {
	t.Helper()
	out, err := Render(cfg, extras, opts)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	objs := map[string]object{}
	first := true
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return objs
		}
		if err != nil {
			t.Fatalf("invalid YAML in rendered manifests: %v", err)
		}
		if first && !opts.SkipNamespace && o.Kind != "Namespace" {
			t.Errorf("first object is %s, want Namespace", o.Kind)
		}
		first = false
		key := o.Kind + "/" + o.Metadata.Name
		if _, dup := objs[key]; dup {
			t.Errorf("duplicate object %s", key)
		}
		objs[key] = o
	}
}

func TestRender(t *testing.T) {
	cfg := testConfig()
	objs := render(t, cfg, stack.Extras{}, Options{})

	for key, o := range objs {
		if o.APIVersion == "" || o.Kind == "" || o.Metadata.Name == "" {
			t.Errorf("incomplete object %s", key)
		}
		if !clusterScoped[o.Kind] && o.Metadata.Namespace != "mon" {
			t.Errorf("%s in namespace %q, want mon", key, o.Metadata.Namespace)
		}
		if clusterScoped[o.Kind] && o.Metadata.Namespace != "" {
			t.Errorf("cluster-scoped %s has a namespace", key)
		}
		for _, v := range o.Spec.VolumeClaimTemplates {
			if v.Spec.StorageClassName != "fast" {
				t.Errorf("%s storageClassName = %q", key, v.Spec.StorageClassName)
			}
		}
		// Every workload container has requests and a memory limit.
		for _, c := range o.Spec.Template.Spec.Containers {
			if c.Resources.Requests["memory"] == "" || c.Resources.Limits["memory"] == "" {
				t.Errorf("%s container %s has no memory request/limit", key, c.Name)
			}
			if c.Image == "" {
				t.Errorf("%s container %s has no image", key, c.Name)
			}
		}
	}

	for _, k := range []string{
		"StatefulSet/prometheus", "Deployment/alertmanager", "StatefulSet/grafana",
		"DaemonSet/node-exporter", "Deployment/kube-state-metrics", "Deployment/blackbox",
		"StatefulSet/loki", "DaemonSet/fluent-bit", "StatefulSet/jaeger", "ClusterRole/mon-prometheus",
		"ClusterRole/mon-kube-state-metrics", "ConfigMap/prometheus-rules", "ConfigMap/loki-config", "ConfigMap/jaeger-config",
		"Deployment/gateway", "Service/gateway", "ConfigMap/gateway-config",
	} {
		if _, ok := objs[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if _, ok := objs["Ingress/gateway"]; ok {
		t.Error("Ingress rendered without a host")
	}

	if pw := objs["Secret/grafana-admin"].StringData["password"]; pw != cfg.Grafana.AdminPassword {
		t.Errorf("grafana password = %q", pw)
	}
	if !strings.Contains(objs["ConfigMap/prometheus-config"].Data["prometheus.yml"], "names: ['mon']") {
		t.Error("prometheus.yml not scoped to the namespace")
	}
	rules := objs["ConfigMap/prometheus-rules"].Data
	if !strings.Contains(rules["rules.yml"], "{{ $labels.instance }}") || rules["kubernetes.yml"] == "" {
		t.Errorf("rules ConfigMap keys = %v", keys(rules))
	}
	prom := objs["StatefulSet/prometheus"]
	if !contains(prom.Spec.Template.Spec.Containers[0].Args, "--storage.tsdb.retention.time=720h") {
		t.Errorf("prometheus args = %v", prom.Spec.Template.Spec.Containers[0].Args)
	}
	if got := prom.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests["storage"]; got != "50Gi" {
		t.Errorf("prometheus volume = %q", got)
	}
	if !strings.Contains(objs["Secret/alertmanager-config"].StringData["alertmanager.yml"], "chat_id: 2") {
		t.Error("alertmanager config not rendered")
	}

	// Dashboards: one ConfigMap each, all under the 1 MiB object limit.
	n := 0
	for key, o := range objs {
		if strings.HasPrefix(key, "ConfigMap/grafana-dashboard-") && key != "ConfigMap/grafana-dashboard-provider" {
			n++
			for _, body := range o.Data {
				if len(body) > 1<<20 {
					t.Errorf("%s is %d bytes", key, len(body))
				}
			}
		}
	}
	if n != 2 {
		t.Errorf("found %d dashboard ConfigMaps, want 2", n)
	}
}

func TestRenderIngress(t *testing.T) {
	cfg := testConfig()
	cfg.Kubernetes.Ingress = config.Ingress{Host: "grafana.example.com", ClassName: "nginx", ClusterIssuer: "letsencrypt"}
	objs := render(t, cfg, stack.Extras{}, Options{})
	in, ok := objs["Ingress/gateway"]
	if !ok {
		t.Fatal("no Ingress")
	}
	if in.Spec.IngressClassName != "nginx" || in.Metadata.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("ingress = %+v", in)
	}
	if len(in.Spec.TLS) != 1 || in.Spec.TLS[0].SecretName != "grafana-tls" || in.Spec.TLS[0].Hosts[0] != "grafana.example.com" {
		t.Errorf("tls = %+v", in.Spec.TLS)
	}
	if !hasEnv(objs["StatefulSet/grafana"], "GF_SERVER_ROOT_URL", "https://grafana.example.com/") {
		t.Error("GF_SERVER_ROOT_URL not set to the https URL")
	}

	cfg.Kubernetes.Ingress = config.Ingress{Host: "grafana.example.com"}
	objs = render(t, cfg, stack.Extras{}, Options{})
	if len(objs["Ingress/gateway"].Spec.TLS) != 0 || !hasEnv(objs["StatefulSet/grafana"], "GF_SERVER_ROOT_URL", "http://grafana.example.com/") {
		t.Error("plain-HTTP ingress rendered wrongly")
	}
}

func TestRenderExtras(t *testing.T) {
	extras := stack.Extras{
		Rules:      map[string][]byte{"custom-mine.yml": []byte("groups:\n  - name: mine\n    rules: []\n")},
		Dashboards: map[string][]byte{"custom-My App.json": []byte(`{"title":"x"}`)},
	}
	objs := render(t, testConfig(), extras, Options{})
	if objs["ConfigMap/prometheus-rules"].Data["custom-mine.yml"] == "" {
		t.Error("custom rule missing")
	}
	names, err := DashboardConfigMaps(extras)
	if err != nil || len(names) != 3 {
		t.Fatalf("DashboardConfigMaps = %v, %v", names, err)
	}
	for _, n := range names {
		if _, ok := objs["ConfigMap/"+n]; !ok {
			t.Errorf("dashboard ConfigMap %s not rendered", n)
		}
		if len(n) > 253 || strings.ContainsAny(n, " _ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("invalid ConfigMap name %q", n)
		}
	}
}

func TestRenderSkipNamespace(t *testing.T) {
	for k := range render(t, testConfig(), stack.Extras{}, Options{SkipNamespace: true}) {
		if strings.HasPrefix(k, "Namespace/") {
			t.Error("Namespace rendered with SkipNamespace")
		}
	}
}

func TestConfigChangeRollsPods(t *testing.T) {
	a, _ := Render(testConfig(), stack.Extras{}, Options{})
	cfg := testConfig()
	cfg.Notifications.Telegram.ChatID = "3"
	cfg.Retention.Logs = "7d"
	b, _ := Render(cfg, stack.Extras{}, Options{})
	sum := func(out []byte, kind string) string {
		i := bytes.Index(out, []byte(kind))
		j := bytes.Index(out[i:], []byte("checksum/config: "))
		return string(out[i+j : i+j+40])
	}
	for _, kind := range []string{"kind: Deployment\nmetadata:\n  name: alertmanager", "kind: StatefulSet\nmetadata:\n  name: loki"} {
		if sum(a, kind) == sum(b, kind) {
			t.Errorf("checksum unchanged after config change for %q", kind)
		}
	}
}

func TestRenderRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Kubernetes.Namespace = "Bad_NS"
	if _, err := Render(cfg, stack.Extras{}, Options{}); err == nil {
		t.Error("expected validation error")
	}
}

func hasEnv(o object, name, value string) bool {
	for _, c := range o.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == name && e.Value == value {
				return true
			}
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fake returns a placeholder credential for tests. It is built at runtime so
// secret scanners don't mistake test fixtures for real credentials.
func fake(s string) string { return "fake-" + s }

func TestRenderGatewayAndStatusDomain(t *testing.T) {
	cfg := testConfig()
	cfg.Probes = []string{"https://shop.example.com"}
	cfg.Gateway = config.Gateway{Domain: "mon.example.com", StatusDomain: "status.example.com"}
	cfg.StatusPage = config.StatusPage{Enabled: true, Title: "Acme", Monitors: []config.Monitor{{Probe: "https://shop.example.com", Name: "Shop"}}}
	cfg.Kubernetes.Ingress = config.Ingress{ClusterIssuer: "le"}
	objs := render(t, cfg, stack.Extras{}, Options{})

	in := objs["Ingress/gateway"]
	if len(in.Spec.Rules) != 2 || in.Spec.Rules[0].Host != "mon.example.com" || in.Spec.Rules[1].Host != "status.example.com" {
		t.Fatalf("ingress rules = %+v", in.Spec.Rules)
	}
	for _, r := range in.Spec.Rules {
		if r.HTTP.Paths[0].Backend.Service.Name != "gateway" {
			t.Errorf("host %s routes to %q, want gateway", r.Host, r.HTTP.Paths[0].Backend.Service.Name)
		}
	}
	if len(in.Spec.TLS) != 1 || len(in.Spec.TLS[0].Hosts) != 2 {
		t.Errorf("tls = %+v", in.Spec.TLS)
	}
	gw := objs["ConfigMap/gateway-config"].Data
	if !strings.Contains(gw["Caddyfile"], "http://status.example.com") || strings.Contains(gw["Caddyfile"], "tls internal") {
		t.Errorf("k8s Caddyfile:\n%s", gw["Caddyfile"])
	}
	if !strings.Contains(gw["config.json"], `"name": "Shop"`) || !strings.Contains(gw["index.html"], "<title>") {
		t.Error("status page files missing from the gateway ConfigMap")
	}
	if !hasEnv(objs["StatefulSet/grafana"], "GF_SERVER_ROOT_URL", "https://mon.example.com/") {
		t.Error("GF_SERVER_ROOT_URL not set from gateway.domain")
	}
}
