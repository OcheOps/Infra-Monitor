// Package integrations monitors common services (databases, caches, web
// servers, queues): it finds them, and renders the exporter containers and
// scrape targets that collect their metrics.
package integrations

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
)

// Spec describes one supported service type.
type Spec struct {
	Type  string
	Title string
	// DefaultPort is the service's usual port.
	DefaultPort int
	// ExporterPort is where the exporter serves metrics; 0 means the
	// service exposes Prometheus metrics itself and is scraped directly.
	ExporterPort int
	// MetricsPath for services scraped directly.
	MetricsPath string
	// DefaultUser and DefaultDatabase fill in omitted credentials.
	DefaultUser, DefaultDatabase string
	// Images are container image names that identify the service.
	Images []string
	Hint   string
}

var Specs = map[string]Spec{
	"postgres": {Type: "postgres", Title: "PostgreSQL", DefaultPort: 5432, ExporterPort: 9187,
		DefaultUser: "postgres", DefaultDatabase: "postgres",
		Images: []string{"postgres", "postgis", "postgresql", "timescaledb"},
		Hint:   "A read-only user works: CREATE USER exporter PASSWORD '...'; GRANT pg_monitor TO exporter;"},
	"mysql": {Type: "mysql", Title: "MySQL / MariaDB", DefaultPort: 3306, ExporterPort: 9104,
		DefaultUser: "exporter",
		Images:      []string{"mysql", "mariadb", "percona-server"},
		Hint:        "CREATE USER 'exporter'@'%' IDENTIFIED BY '...'; GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';"},
	"redis": {Type: "redis", Title: "Redis", DefaultPort: 6379, ExporterPort: 9121,
		Images: []string{"redis", "redis-stack", "valkey", "keydb"}},
	"mongodb": {Type: "mongodb", Title: "MongoDB", DefaultPort: 27017, ExporterPort: 9216,
		Images: []string{"mongo", "mongodb"},
		Hint:   "Needs a user with the clusterMonitor role if authentication is on."},
	"nginx": {Type: "nginx", Title: "Nginx", DefaultPort: 80, ExporterPort: 9113, MetricsPath: "/stub_status",
		Images: []string{"nginx", "nginx-unprivileged"},
		Hint:   "Needs stub_status enabled: location = /stub_status { stub_status; allow 172.16.0.0/12; deny all; }"},
	"rabbitmq": {Type: "rabbitmq", Title: "RabbitMQ", DefaultPort: 15692, MetricsPath: "/metrics",
		Images: []string{"rabbitmq"},
		Hint:   "Uses RabbitMQ's built-in Prometheus plugin (rabbitmq-plugins enable rabbitmq_prometheus)."},
}

// Types lists supported types in a stable order.
func Types() []string {
	t := make([]string, 0, len(Specs))
	for k := range Specs {
		t = append(t, k)
	}
	sort.Strings(t)
	return t
}

// Images maps each type that needs an exporter to its pinned image, read
// from deploy/integrations/catalog.yml (which Dependabot keeps current).
var Images = func() map[string]string {
	data, err := deploy.FS.ReadFile("integrations/catalog.yml")
	if err != nil {
		panic(err)
	}
	var f struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		panic(fmt.Sprintf("deploy/integrations/catalog.yml: %v", err))
	}
	out := map[string]string{}
	for k, v := range f.Services {
		out[k] = v.Image
	}
	return out
}()

// ServiceName is the exporter's Compose service and scrape job name.
func ServiceName(name string) string { return "integration-" + name }

// Target is one scrape target for an integration.
type Target struct {
	Job, Address, Path string
}

// Targets returns what Prometheus scrapes for the integrations.
func Targets(ints []config.Integration) []Target {
	var out []Target
	for _, in := range ints {
		spec := Specs[in.Type]
		t := Target{Job: ServiceName(in.Name)}
		if spec.ExporterPort == 0 {
			t.Address = in.Address
			t.Path = spec.MetricsPath
		} else {
			t.Address = fmt.Sprintf("%s:%d", ServiceName(in.Name), spec.ExporterPort)
		}
		out = append(out, t)
	}
	return out
}

// ComposeFile renders the exporter services as an extra Compose file, plus
// an env file per exporter holding its credentials. network is the stack's
// own network key ("monitor" on servers, "agent" on agents). It returns nil
// when no integration needs an exporter.
func ComposeFile(ints []config.Integration, network string) ([]byte, map[string][]byte, error) {
	type service struct {
		Image      string   `yaml:"image"`
		Restart    string   `yaml:"restart"`
		Networks   []string `yaml:"networks"`
		EnvFile    []string `yaml:"env_file,omitempty"`
		Command    []string `yaml:"command,omitempty"`
		ExtraHosts []string `yaml:"extra_hosts"`
	}
	type extNetwork struct {
		External bool   `yaml:"external"`
		Name     string `yaml:"name"`
	}
	services := map[string]service{}
	networks := map[string]extNetwork{}
	envs := map[string][]byte{}

	for _, in := range ints {
		spec := Specs[in.Type]
		if spec.ExporterPort == 0 {
			continue
		}
		svc := service{
			Image:    Images[in.Type],
			Restart:  "unless-stopped",
			Networks: []string{network},
			// Lets the exporter reach services installed on the host itself.
			ExtraHosts: []string{"host.docker.internal:host-gateway"},
		}
		if in.Network != "" {
			key := "ext-" + regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(in.Network, "-")
			networks[key] = extNetwork{External: true, Name: in.Network}
			svc.Networks = append(svc.Networks, key)
		}
		env, cmd := exporterSettings(in, spec)
		svc.Command = cmd
		if len(env) > 0 {
			var b strings.Builder
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				// Single quotes keep Compose from expanding `$` in values;
				// validation forbids quotes and newlines in credentials.
				fmt.Fprintf(&b, "%s='%s'\n", k, env[k])
			}
			envs[in.Name+".env"] = []byte(b.String())
			svc.EnvFile = []string{"./integrations/" + in.Name + ".env"}
		}
		services[ServiceName(in.Name)] = svc
	}
	if len(services) == 0 {
		return nil, nil, nil
	}
	doc := map[string]any{"services": services}
	if len(networks) > 0 {
		doc["networks"] = networks
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	header := "# Generated by infra-monitor from the `integrations` in config.yaml.\n"
	return append([]byte(header), out...), envs, nil
}

// exporterSettings returns the exporter's environment and arguments.
func exporterSettings(in config.Integration, spec Spec) (map[string]string, []string) {
	user := in.Username
	if user == "" {
		user = spec.DefaultUser
	}
	db := in.Database
	if db == "" {
		db = spec.DefaultDatabase
	}
	env := map[string]string{}
	var cmd []string
	switch in.Type {
	case "postgres":
		env["DATA_SOURCE_URI"] = in.Address + "/" + db + "?sslmode=disable"
		env["DATA_SOURCE_USER"] = user
		env["DATA_SOURCE_PASS"] = in.Password
	case "mysql":
		cmd = []string{"--mysqld.address=" + in.Address, "--mysqld.username=" + user}
		env["MYSQLD_EXPORTER_PASSWORD"] = in.Password
	case "redis":
		env["REDIS_ADDR"] = "redis://" + in.Address
		if in.Username != "" {
			env["REDIS_USER"] = in.Username
		}
		if in.Password != "" {
			env["REDIS_PASSWORD"] = in.Password
		}
	case "mongodb":
		u := url.URL{Scheme: "mongodb", Host: in.Address, Path: "/admin", RawQuery: "directConnection=true"}
		if in.Username != "" {
			u.User = url.UserPassword(in.Username, in.Password)
		}
		env["MONGODB_URI"] = u.String()
		cmd = []string{"--collector.diagnosticdata"}
	case "nginx":
		path := spec.MetricsPath
		if in.Path != "" {
			path = in.Path
		}
		cmd = []string{"--nginx.scrape-uri=http://" + in.Address + path}
	}
	return env, cmd
}

// Candidate is a service found on this host that could be monitored.
type Candidate struct {
	Type    string
	Name    string // suggested integration name
	Address string // as reachable from an exporter container
	Network string // Docker network to join, if any
	Source  string // where it was found, for display
	Warning string
}

// Runner runs a command and returns its output.
type Runner func(name string, args ...string) (string, error)

var nonName = regexp.MustCompile(`[^a-z0-9_-]+`)

// Detect finds supported services in running containers and among the
// host's listening ports. ownProjects are Compose projects to ignore.
func Detect(run Runner, procNetTCP []string, ownProjects []string) []Candidate {
	var found []Candidate
	published := map[int]bool{}

	out, err := run("docker", "ps", "--format", `{{.Names}}	{{.Image}}	{{.Label "com.docker.compose.project"}}`)
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) < 2 || parts[0] == "" {
				continue
			}
			name, image := parts[0], parts[1]
			if len(parts) > 2 && slicesContains(ownProjects, parts[2]) {
				continue
			}
			typ := typeForImage(image)
			if typ == "" {
				continue
			}
			spec := Specs[typ]
			c := Candidate{Type: typ, Name: suggestName(name), Source: "container " + name + " (" + image + ")"}
			nets, _ := run("docker", "inspect", "-f", `{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}`, name)
			for _, n := range strings.Fields(nets) {
				if n != "host" && n != "none" && n != "bridge" {
					c.Network = n
					break
				}
			}
			if c.Network != "" {
				c.Address = fmt.Sprintf("%s:%d", name, spec.DefaultPort)
			} else if port := publishedPort(run, name, spec.DefaultPort); port != 0 {
				c.Address = fmt.Sprintf("host.docker.internal:%d", port)
				published[port] = true
			} else {
				c.Address = fmt.Sprintf("%s:%d", name, spec.DefaultPort)
				c.Warning = "it's on Docker's default network and publishes no port, so an exporter can't reach it; put it on a user-defined network"
			}
			if typ == "nginx" {
				c.Warning = spec.Hint
			}
			found = append(found, c)
		}
	}

	for _, l := range listening(procNetTCP) {
		if published[l.port] {
			continue
		}
		for _, typ := range Types() {
			spec := Specs[typ]
			if spec.DefaultPort != l.port || typ == "nginx" { // port 80 is too ambiguous
				continue
			}
			if slicesContainsFunc(found, func(c Candidate) bool { return c.Type == typ && strings.HasPrefix(c.Address, "host.docker.internal:") }) {
				continue
			}
			c := Candidate{Type: typ, Name: typ, Address: fmt.Sprintf("host.docker.internal:%d", l.port),
				Source: fmt.Sprintf("listening on port %d", l.port)}
			if l.loopbackOnly {
				c.Warning = "it only listens on 127.0.0.1, which containers can't reach; make it listen on the Docker bridge (e.g. listen_addresses / bind) too"
			}
			found = append(found, c)
		}
	}
	return found
}

// typeForImage maps an image reference like "docker.io/library/postgres:16"
// to an integration type.
func typeForImage(image string) string {
	ref := image
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	base := ref[strings.LastIndex(ref, "/")+1:]
	for _, typ := range Types() {
		for _, img := range Specs[typ].Images {
			if base == img {
				return typ
			}
		}
	}
	return ""
}

func suggestName(container string) string {
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(container), "-"), "-_")
	if len(n) > 50 {
		n = n[:50]
	}
	if n == "" {
		n = "service"
	}
	return n
}

func publishedPort(run Runner, container string, port int) int {
	out, err := run("docker", "port", container, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		}
	}
	return 0
}

type listener struct {
	port         int
	loopbackOnly bool
}

// listening parses /proc/net/tcp and /proc/net/tcp6 contents into the
// ports in LISTEN state, noting ports bound only to loopback.
func listening(files []string) []listener {
	any := map[int]bool{}
	loop := map[int]bool{}
	for _, content := range files {
		sc := bufio.NewScanner(strings.NewReader(content))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 4 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			addr, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			if addr == "0100007F" || addr == "00000000000000000000000001000000" {
				loop[int(port)] = true
			} else {
				any[int(port)] = true
			}
		}
	}
	var out []listener
	seen := map[int]bool{}
	for p := range any {
		out = append(out, listener{port: p})
		seen[p] = true
	}
	for p := range loop {
		if !seen[p] {
			out = append(out, listener{port: p, loopbackOnly: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].port < out[j].port })
	return out
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func slicesContainsFunc(s []Candidate, f func(Candidate) bool) bool {
	for _, x := range s {
		if f(x) {
			return true
		}
	}
	return false
}
