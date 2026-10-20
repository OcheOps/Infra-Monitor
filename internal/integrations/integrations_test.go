package integrations

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/OcheOps/Infra-Monitor/internal/config"
)

func TestTypesMatchConfig(t *testing.T) {
	if !reflect.DeepEqual(Types(), config.IntegrationTypes) {
		t.Errorf("integrations.Types() = %v, config.IntegrationTypes = %v", Types(), config.IntegrationTypes)
	}
	for _, typ := range Types() {
		if Specs[typ].ExporterPort != 0 && Images[typ] == "" {
			t.Errorf("%s needs an exporter but deploy/integrations/catalog.yml has no image for it", typ)
		}
	}
}

func TestTargets(t *testing.T) {
	got := Targets([]config.Integration{
		{Type: "postgres", Name: "db", Address: "pg:5432"},
		{Type: "rabbitmq", Name: "mq", Address: "rabbit:15692"},
	})
	want := []Target{
		{Job: "integration-db", Address: "integration-db:9187"},
		{Job: "integration-mq", Address: "rabbit:15692", Path: "/metrics"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Targets = %+v", got)
	}
}

func TestComposeFile(t *testing.T) {
	file, envs, err := ComposeFile([]config.Integration{
		{Type: "postgres", Name: "db", Address: "pg:5432", Network: "shop_default", Password: fake("pa$$")},
		{Type: "redis", Name: "cache", Address: "host.docker.internal:6379"},
		{Type: "rabbitmq", Name: "mq", Address: "rabbit:15692"},
	}, "monitor")
	if err != nil {
		t.Fatal(err)
	}
	s := string(file)
	for _, want := range []string{"integration-db:", "integration-cache:", "prometheuscommunity/postgres-exporter:", "external: true", "name: shop_default", "host.docker.internal:host-gateway", "./integrations/db.env"} {
		if !strings.Contains(s, want) {
			t.Errorf("compose file missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "integration-mq") {
		t.Error("RabbitMQ is scraped directly and needs no exporter")
	}
	if strings.Contains(s, fake("pa$$")) {
		t.Error("password written into the compose file instead of the env file")
	}
	if got := string(envs["db.env"]); got != "DATA_SOURCE_PASS='"+fake("pa$$")+"'\nDATA_SOURCE_URI='pg:5432/postgres?sslmode=disable'\nDATA_SOURCE_USER='postgres'\n" {
		t.Errorf("db.env = %q", got)
	}
	if got := string(envs["cache.env"]); got != "REDIS_ADDR='redis://host.docker.internal:6379'\n" {
		t.Errorf("cache.env = %q", got)
	}

	if file, _, _ := ComposeFile([]config.Integration{{Type: "rabbitmq", Name: "mq", Address: "r:15692"}}, "monitor"); file != nil {
		t.Error("no exporter needed, but a compose file was rendered")
	}
}

func TestDetect(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(cmd, "docker ps"):
			return "shop-db-1\tpostgres:16-alpine\tshop\n" +
				"cache\tdocker.io/library/redis:7\t\n" +
				"lonely\tmongo:7\t\n" +
				"infra-monitor-prometheus-1\tprom/prometheus:v2.54.1\tinfra-monitor\n" +
				"web\tnginx@sha256:abc\t\n", nil
		case cmd == "docker inspect -f {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}} shop-db-1":
			return "shop_default \n", nil
		case strings.HasPrefix(cmd, "docker inspect"):
			return "bridge \n", nil
		case cmd == "docker port cache 6379/tcp":
			return "0.0.0.0:16379\n[::]:16379\n", nil
		case strings.HasPrefix(cmd, "docker port web"):
			return "0.0.0.0:8080\n", nil
		case strings.HasPrefix(cmd, "docker port"):
			return "", fmt.Errorf("no public port")
		}
		t.Fatalf("unexpected command %q", cmd)
		return "", nil
	}
	// MySQL on 0.0.0.0:3306, RabbitMQ metrics on 127.0.0.1:15692, and the
	// Redis container's published port (already found via Docker).
	procNet := []string{`  sl  local_address rem_address   st
   0: 00000000:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0
   1: 0100007F:3D4C 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0
   2: 00000000:3FFB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0
   3: 0100007F:1F90 0100007F:0001 01 00000000:00000000 00:00000000 00000000     0
`}
	got := Detect(run, procNet, []string{"infra-monitor"})
	summary := map[string]string{}
	for _, c := range got {
		summary[c.Name] = c.Type + " " + c.Address + " " + c.Network
		if (c.Name == "lonely" || c.Name == "rabbitmq" || c.Name == "web") != (c.Warning != "") {
			t.Errorf("%s: warning %q", c.Name, c.Warning)
		}
	}
	want := map[string]string{
		"shop-db-1": "postgres shop-db-1:5432 shop_default",
		"cache":     "redis host.docker.internal:16379 ",
		"lonely":    "mongodb lonely:27017 ",
		"web":       "nginx host.docker.internal:8080 ",
		"mysql":     "mysql host.docker.internal:3306 ",
		"rabbitmq":  "rabbitmq host.docker.internal:15692 ",
	}
	if !reflect.DeepEqual(summary, want) {
		t.Errorf("Detect:\n got %v\nwant %v", summary, want)
	}
}

// fake marks test credentials (and keeps secret scanners quiet).
func fake(s string) string { return "fake-" + s }
