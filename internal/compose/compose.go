// Package compose runs the stack on a single host with Docker Compose.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/OcheOps/Infra-Monitor/deploy"
	"github.com/OcheOps/Infra-Monitor/internal/config"
	"github.com/OcheOps/Infra-Monitor/internal/integrations"
	"github.com/OcheOps/Infra-Monitor/internal/stack"
)

// Compose project names. They differ so a server and an agent can share a
// host (CI does, to test them together).
const (
	ProjectName      = "infra-monitor"
	AgentProjectName = "infra-monitor-agent"
)

// EnvFile is the generated Compose env file inside the project directory.
const EnvFile = ".env"

// managedDirs hold only files the CLI writes; anything else in them is
// removed on sync, so deleted custom rules and dashboards disappear.
var managedDirs = []string{"alerts", "provisioning/dashboards", "gateway/status", "integrations"}

// IntegrationsFile is the generated Compose file holding exporter services.
const IntegrationsFile = "integrations.yml"

// Sync writes the compose project into dir, replacing files the CLI owns.
// It reports whether a file that already existed changed, meaning running
// containers need a restart to pick it up.
func Sync(dir string, cfg config.Config, extras stack.Extras) (changed bool, err error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	d, err := stack.NewData(cfg)
	if err != nil {
		return false, err
	}
	files := map[string][]byte{}

	// Static files from the compose bundle; *.tmpl files are rendered below.
	err = fs.WalkDir(deploy.FS, "compose", func(p string, ent fs.DirEntry, err error) error {
		if err != nil || ent.IsDir() || strings.HasSuffix(p, ".tmpl") {
			return err
		}
		b, err := deploy.FS.ReadFile(p)
		files[strings.TrimPrefix(p, "compose/")] = b
		return err
	})
	if err != nil {
		return false, err
	}
	if files["provisioning/datasources/datasources.yml"], err = deploy.FS.ReadFile("shared/grafana/datasources/datasources.yml"); err != nil {
		return false, err
	}
	if files["provisioning/dashboards/dashboard.yml"], err = deploy.FS.ReadFile("shared/grafana/dashboards/dashboard.yml"); err != nil {
		return false, err
	}

	for name, render := range map[string]func() ([]byte, error){
		"config/prometheus.yml":      func() ([]byte, error) { return stack.PrometheusConfig(stack.Compose, d) },
		"config/alertmanager.yml":    func() ([]byte, error) { return stack.AlertmanagerConfig(cfg) },
		"config/loki.yaml":           func() ([]byte, error) { return stack.LokiConfig(d) },
		"config/jaeger.yaml":         func() ([]byte, error) { return stack.JaegerConfig(d) },
		"gateway/Caddyfile":          func() ([]byte, error) { return stack.GatewayConfig(stack.Compose, cfg) },
		"gateway/status/config.json": func() ([]byte, error) { return stack.StatusPageJSON(cfg) },
		"gateway/status/index.html": func() ([]byte, error) {
			return deploy.FS.ReadFile("shared/gateway/status/index.html")
		},
	} {
		b, err := render()
		if err != nil {
			return false, fmt.Errorf("rendering %s: %w", name, err)
		}
		files[name] = b
	}

	rules, err := stack.Rules(stack.Compose, extras, cfg.Agents)
	if err != nil {
		return false, err
	}
	for k, v := range rules {
		files["alerts/"+k] = v
	}
	dashboards, err := stack.Dashboards(extras)
	if err != nil {
		return false, err
	}
	for k, v := range dashboards {
		files["provisioning/dashboards/"+k] = v
	}
	intFile, intEnvs, err := integrations.ComposeFile(cfg.Integrations, "monitor")
	if err != nil {
		return false, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	for rel, data := range files {
		c, err := writeFile(dir, rel, data, 0o644)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}
	gwHTTP, gwHTTPS, rootURL := "127.0.0.1:8081", "127.0.0.1:8443", "http://localhost:3000/"
	if dom := cfg.Gateway.Domain; dom != "" {
		gwHTTP, gwHTTPS, rootURL = "80", "443", "https://"+dom+"/"
	}
	if err := writeIntegrations(dir, intFile, intEnvs, &changed); err != nil {
		return changed, err
	}
	env := fmt.Sprintf("GRAFANA_ADMIN_PASSWORD='%s'\nPROMETHEUS_RETENTION=%dh\nGATEWAY_HTTP=%s\nGATEWAY_HTTPS=%s\nGRAFANA_ROOT_URL=%s\n",
		cfg.Grafana.AdminPassword, d.Retention.Metrics, gwHTTP, gwHTTPS, rootURL)
	if _, err := writeFile(dir, EnvFile, []byte(env), 0o600); err != nil {
		return changed, err
	}

	for name := range intEnvs {
		files["integrations/"+name] = nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "integrations"), 0o700); err != nil {
		return changed, err
	}
	for _, md := range managedDirs {
		entries, err := os.ReadDir(filepath.Join(dir, md))
		if err != nil {
			return changed, err
		}
		for _, ent := range entries {
			if _, keep := files[path.Join(md, ent.Name())]; !keep {
				if err := os.RemoveAll(filepath.Join(dir, md, ent.Name())); err != nil {
					return changed, err
				}
				changed = true
			}
		}
	}
	return changed, nil
}

// writeFile writes data if it differs from what is on disk and reports
// whether an existing file was changed.
func writeFile(dir, rel string, data []byte, mode os.FileMode) (bool, error) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	old, err := os.ReadFile(p)
	existed := err == nil
	switch {
	case existed && bytes.Equal(old, data):
		return false, os.Chmod(p, mode)
	case !existed && !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	// Containers run as non-root users and must be able to read config
	// files; the 0700 project directory keeps other host users out.
	if err := os.WriteFile(p, data, mode); err != nil {
		return false, err
	}
	return existed, os.Chmod(p, mode)
}

// Command builds a `docker compose` invocation for the project in dir,
// including the generated integrations file when there is one.
func Command(project, dir string, args ...string) *exec.Cmd {
	full := []string{
		"compose",
		"--project-name", project,
		"--project-directory", dir,
		"--file", filepath.Join(dir, "docker-compose.yml"),
	}
	if _, err := os.Stat(filepath.Join(dir, IntegrationsFile)); err == nil {
		full = append(full, "--file", filepath.Join(dir, IntegrationsFile))
	}
	full = append(full, "--env-file", filepath.Join(dir, EnvFile))
	full = append(full, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// SyncAgent writes an agent host's Compose project into dir. ca is the
// server's CA certificate when it is pinned (`expose --tls internal`).
func SyncAgent(dir string, cfg config.Config, ca []byte) (changed bool, err error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	if cfg.Agent == nil {
		return false, errors.New("this host is not an agent")
	}
	ag := *cfg.Agent
	files, err := stack.AgentFiles(ag, cfg.Integrations, ca != nil)
	if err != nil {
		return false, err
	}
	compose, err := deploy.FS.ReadFile("agent/docker-compose.yml")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	write := func(rel string, data []byte, mode os.FileMode) error {
		c, err := writeFile(dir, rel, data, mode)
		changed = changed || c
		return err
	}
	if err := write("docker-compose.yml", compose, 0o644); err != nil {
		return changed, err
	}
	for name, data := range files {
		// The containers run as non-root users and read these through the
		// 0700 project directory.
		if err := write("config/"+name, data, 0o644); err != nil {
			return changed, err
		}
	}
	if ca != nil {
		if err := write("config/ca.crt", ca, 0o644); err != nil {
			return changed, err
		}
	} else if err := os.Remove(filepath.Join(dir, "config", "ca.crt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return changed, err
	}

	intFile, intEnvs, err := integrations.ComposeFile(cfg.Integrations, "agent")
	if err != nil {
		return changed, err
	}
	if err := writeIntegrations(dir, intFile, intEnvs, &changed); err != nil {
		return changed, err
	}
	if err := pruneDir(filepath.Join(dir, "integrations"), intEnvs, &changed); err != nil {
		return changed, err
	}

	env := ""
	if ag.ServerIP != "" {
		u, err := url.Parse(ag.Server)
		if err != nil {
			return changed, err
		}
		env = fmt.Sprintf("SERVER_HOST=%s\nSERVER_IP=%s\n", u.Hostname(), ag.ServerIP)
	}
	if err := write(EnvFile, []byte(env), 0o600); err != nil {
		return changed, err
	}
	return changed, nil
}

// writeIntegrations writes (or removes) the integrations Compose file and the
// exporters' credential env files, which only the owner can read.
func writeIntegrations(dir string, file []byte, envs map[string][]byte, changed *bool) error {
	path := filepath.Join(dir, IntegrationsFile)
	if file == nil {
		if err := os.Remove(path); err == nil {
			*changed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		c, err := writeFile(dir, IntegrationsFile, file, 0o644)
		if err != nil {
			return err
		}
		*changed = *changed || c
	}
	if err := os.MkdirAll(filepath.Join(dir, "integrations"), 0o700); err != nil {
		return err
	}
	for name, data := range envs {
		c, err := writeFile(dir, "integrations/"+name, data, 0o600)
		if err != nil {
			return err
		}
		*changed = *changed || c
	}
	return nil
}

// pruneDir removes files in dir that aren't in keep.
func pruneDir(dir string, keep map[string][]byte, changed *bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if _, ok := keep[ent.Name()]; !ok {
			if err := os.RemoveAll(filepath.Join(dir, ent.Name())); err != nil {
				return err
			}
			*changed = true
		}
	}
	return nil
}
